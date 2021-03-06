package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"

	"github.com/alibaba/pouch/apis/types"
	"github.com/alibaba/pouch/client"
	"github.com/alibaba/pouch/pkg/ioutils"
	"github.com/alibaba/pouch/pkg/log"

	"github.com/docker/docker/pkg/stdcopy"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/sys/unix"
)

// execDescription is used to describe exec command in detail and auto generate command doc.
var execDescription = "Run a command in a running container"

// ExecCommand is used to implement 'exec' command.
type ExecCommand struct {
	baseCommand
	Interactive bool
	Terminal    bool
	Detach      bool
	User        string
	Envs        []string
	Privileged  bool
}

// Init initializes ExecCommand command.
func (e *ExecCommand) Init(c *Cli) {
	e.cli = c
	e.cmd = &cobra.Command{
		Use:   "exec [OPTIONS] CONTAINER COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Long:  execDescription,
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return e.runExec(args)
		},
		Example: execExample(),
	}
	e.addFlags()
}

// addFlags adds flags for specific command.
func (e *ExecCommand) addFlags() {
	flagSet := e.cmd.Flags()
	flagSet.SetInterspersed(false)
	flagSet.BoolVarP(&e.Detach, "detach", "d", false, "Run the process in the background")
	flagSet.BoolVarP(&e.Terminal, "tty", "t", false, "Allocate a tty device")
	flagSet.BoolVarP(&e.Interactive, "interactive", "i", false, "Open container's STDIN")
	flagSet.StringVarP(&e.User, "user", "u", "", "Username or UID (format: <name|uid>[:<group|gid>])")
	flagSet.StringArrayVarP(&e.Envs, "env", "e", []string{}, "Set environment variables")
	flagSet.BoolVar(&e.Privileged, "privileged", false, "Give extended privileges to the exec process")
}

// runExec is the entry of ExecCommand command.
func (e *ExecCommand) runExec(args []string) error {
	ctx := context.Background()
	apiClient := e.cli.Client()

	// create exec process.
	id := args[0]
	command := args[1:]

	createExecConfig := &types.ExecCreateConfig{
		Cmd:          command,
		Tty:          e.Terminal,
		Detach:       e.Detach,
		AttachStderr: !e.Detach,
		AttachStdout: !e.Detach,
		AttachStdin:  !e.Detach && e.Interactive,
		Privileged:   e.Privileged,
		User:         e.User,
		Env:          e.Envs,
	}

	if err := checkTty(createExecConfig.AttachStdin, createExecConfig.Tty, os.Stdin.Fd()); err != nil {
		return err
	}

	createResp, err := apiClient.ContainerCreateExec(ctx, id, createExecConfig)
	if err != nil {
		return fmt.Errorf("failed to create exec: %v", err)
	}

	// start exec process.
	startExecConfig := &types.ExecStartConfig{
		Detach: e.Detach,
		Tty:    e.Terminal,
	}

	conn, reader, err := apiClient.ContainerStartExec(ctx, createResp.ID, startExecConfig)
	if err != nil {
		return fmt.Errorf("failed to start exec: %v", err)
	}

	if e.Detach {
		return nil
	}

	// handle stdio.
	if err := holdHijackConnection(ctx, apiClient, createResp.ID, conn, reader, createExecConfig.AttachStdin, createExecConfig.AttachStdout, createExecConfig.AttachStderr, e.Terminal); err != nil {
		return err
	}

	execInfo, err := apiClient.ContainerExecInspect(ctx, createResp.ID)
	if err != nil {
		return err
	}

	code := execInfo.ExitCode
	if code != 0 {
		return ExitError{Code: int(code)}
	}

	return nil
}

func holdHijackConnection(ctx context.Context, apiClient client.CommonAPIClient, execID string, conn net.Conn, reader *bufio.Reader, stdin, stdout, stderr, tty bool) error {
	if stdin && tty {
		in, out, err := setRawMode(true, false)
		if err != nil {
			return fmt.Errorf("failed to set raw mode")
		}
		defer func() {
			if err := restoreMode(in, out); err != nil {
				fmt.Fprintf(os.Stderr, "failed to restore term mode")
			}
		}()
	}

	stdoutDone := make(chan error, 1)
	go func() {
		var err error
		if stderr || stdout {
			if !tty {
				_, err = stdcopy.StdCopy(os.Stdout, os.Stderr, reader)
			} else {
				_, err = io.Copy(os.Stdout, reader)
			}
		}
		stdoutDone <- err
	}()

	stdinDone := make(chan struct{})
	go func() {
		if stdin {
			io.Copy(conn, os.Stdin)
			// close write if receive CTRL-D
			if cw, ok := conn.(ioutils.CloseWriter); ok {
				cw.CloseWrite()
			}
		}

		close(stdinDone)
	}()

	// resize exec tty
	if tty {
		if err := execResize(ctx, apiClient, execID); err != nil {
			return err
		}
	}

	select {
	case err := <-stdoutDone:
		if err != nil {
			log.With(ctx).Debugf("receive stdout error: %s", err)
			return err
		}

	case <-stdinDone:
		if stdout || stderr {
			select {
			case err := <-stdoutDone:
				log.With(ctx).Debugf("receive stdout error: %s", err)
				return err
			case <-ctx.Done():
			}
		}

	case <-ctx.Done():
	}

	return nil
}

func execResize(ctx context.Context, apiClient client.CommonAPIClient, execID string) error {
	width, height, err := terminal.GetSize(int(os.Stdin.Fd()))
	if err != nil {
		return err
	}

	// retry first resize exec.
	for i := 0; i < 16; i++ {
		err = apiClient.ContainerExecResize(ctx, execID, types.ResizeOptions{Width: int64(width), Height: int64(height)})
		if err == nil {
			break
		}
	}

	s := make(chan os.Signal, 16)
	signal.Notify(s, unix.SIGWINCH)
	go func() {
		for range s {
			width, height, err = terminal.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				log.With(ctx).Debugf("failed to get tty size, err(%v)", err)
				continue
			}
			err = apiClient.ContainerExecResize(ctx, execID, types.ResizeOptions{Width: int64(width), Height: int64(height)})
			if err != nil {
				log.With(ctx).Debugf("failed to resize tty, err(%v)", err)
			}
		}
	}()

	return nil
}

// execExample shows examples in exec command, and is used in auto-generated cli docs.
func execExample() string {
	return `$ pouch exec -it 25bf50 ps
PID   USER     TIME  COMMAND
    1 root      0:00 /bin/sh
   38 root      0:00 ps
`
}
