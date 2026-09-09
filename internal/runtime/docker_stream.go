package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/entrypoint"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/moby/term"
)

// copyDockerOutput rejects truncated frames, including EOF halfway through a
// payload. Docker's stdcopy treats some truncated EOFs as success, which is not
// safe for a CLI whose output may feed another program.
func copyDockerOutput(stdout, stderr io.Writer, input io.Reader) error {
	var header [8]byte
	for {
		n, err := io.ReadFull(input, header[:])
		if errors.Is(err, io.EOF) && n == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading Docker stream header: %w", err)
		}
		var output io.Writer
		switch header[0] {
		case 0, 1:
			output = stdout
		case 2:
			output = stderr
		case 3:
			return fmt.Errorf("docker daemon reported a stream error")
		default:
			return fmt.Errorf("invalid Docker stream type %d", header[0])
		}
		size := int64(binary.BigEndian.Uint32(header[4:]))
		if _, err := io.CopyN(output, input, size); err != nil {
			return fmt.Errorf("reading Docker stream payload: %w", err)
		}
	}
}

// stdioIsTTY reports whether both stdin and stdout are terminals; only then
// does debux allocate a remote TTY, mirroring docker/kubectl CLI behavior so
// piped output is not CRLF-mangled and stderr stays separate.
func stdioIsTTY() bool {
	_, stdinIsTerminal := term.GetFdInfo(os.Stdin)
	_, stdoutIsTerminal := term.GetFdInfo(os.Stdout)
	return stdinIsTerminal && stdoutIsTerminal
}

// runInteractiveContainer attaches to a created container, starts it, streams
// I/O (raw terminal mode and TTY resize when stdio is a terminal), waits for
// it to exit, and propagates the container's exit status.
func runInteractiveContainer(ctx context.Context, cli *client.Client, containerID string, tty, autoRemove bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hijacked, err := cli.ContainerAttach(ctx, containerID, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
		Stdout: true,
		Stderr: true,
	})
	if err != nil {
		return fmt.Errorf("attaching to container: %w", err)
	}
	defer hijacked.Close()

	// Register the wait before starting so a container that exits (and, with
	// --rm, is auto-removed) immediately cannot slip past the wait.
	cond := container.WaitConditionNextExit
	if autoRemove {
		cond = container.WaitConditionRemoved
	}
	waitResult := cli.ContainerWait(ctx, containerID, client.ContainerWaitOptions{Condition: cond})

	if _, err := cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting container: %w", err)
	}

	stdinFd, _ := term.GetFdInfo(os.Stdin)
	if tty {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		resizeTTY(ctx, cli, containerID, stdinFd)
	}

	outputDone := make(chan error, 1)
	go func() {
		var err error
		if tty {
			_, err = io.Copy(os.Stdout, hijacked.Reader)
		} else {
			err = copyDockerOutput(os.Stdout, os.Stderr, hijacked.Reader)
		}
		outputDone <- err
	}()

	stopInput, err := startSessionInput(os.Stdin, hijacked.Conn, hijacked.Close, hijacked.CloseWrite)
	if err != nil {
		return err
	}
	defer stopInput()
	stopInterrupt := context.AfterFunc(ctx, hijacked.Close)
	defer stopInterrupt()

	select {
	case err := <-waitResult.Error:
		if err != nil {
			return fmt.Errorf("waiting for container: %w", err)
		}
		return fmt.Errorf("container wait ended without a result")
	case status, ok := <-waitResult.Result:
		if !ok {
			return fmt.Errorf("container wait ended without a result")
		}
		// Let the output goroutine flush the tail before the deferred
		// RestoreTerminal runs.
		select {
		case streamErr := <-outputDone:
			if streamErr != nil {
				return fmt.Errorf("reading container output: %w", streamErr)
			}
		case <-time.After(2 * time.Second):
			return fmt.Errorf("timed out draining container output")
		case <-ctx.Done():
			return ctx.Err()
		}
		if status.Error != nil && status.Error.Message != "" {
			return fmt.Errorf("waiting for container: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return &ExitError{Code: int(status.StatusCode)}
		}
		return nil
	case <-ctx.Done():
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}
}

func resizeTTY(ctx context.Context, cli *client.Client, containerID string, fd uintptr) {
	resize := func() {
		size, err := term.GetWinsize(fd)
		if err != nil || size == nil {
			return
		}
		_, _ = cli.ContainerResize(ctx, containerID, client.ContainerResizeOptions{
			Height: uint(size.Height),
			Width:  uint(size.Width),
		})
	}

	// Initial resize
	resize()

	// Watch for terminal resize signals
	sigCh, stopSig := watchSIGWINCH()
	go func() {
		defer stopSig()
		for {
			select {
			case <-sigCh:
				resize()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// execInContainer starts an interactive zsh session inside a running container
// using docker exec, similar to how K8s uses exec into daemon ephemeral containers.
func execInContainer(ctx context.Context, cli *client.Client, containerID string, command []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := bootstrapDockerShell(ctx, cli, containerID); err != nil {
		return fmt.Errorf("preparing debux shell config: %w", err)
	}

	tty := stdioIsTTY()
	stdinFd, _ := term.GetFdInfo(os.Stdin)

	createOpts := client.ExecCreateOptions{
		Cmd:          debuxExecCommand(command),
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		TTY:          tty,
	}
	if tty {
		// Set the initial size at create time; resizing only after attach
		// races exec start and can leave the session at 80x24.
		if size, err := term.GetWinsize(stdinFd); err == nil && size != nil {
			createOpts.ConsoleSize = client.ConsoleSize{Height: uint(size.Height), Width: uint(size.Width)}
		}
	}

	resp, err := cli.ExecCreate(ctx, containerID, createOpts)
	if err != nil {
		return fmt.Errorf("creating exec session: %w", err)
	}

	hijacked, err := cli.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{
		TTY: tty,
	})
	if err != nil {
		return fmt.Errorf("attaching to exec session: %w", err)
	}
	defer hijacked.Close()

	if tty {
		oldState, err := term.SetRawTerminal(stdinFd)
		if err == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}

		resizeExec := func() {
			size, err := term.GetWinsize(stdinFd)
			if err == nil && size != nil {
				_, _ = cli.ExecResize(ctx, resp.ID, client.ExecResizeOptions{
					Height: uint(size.Height),
					Width:  uint(size.Width),
				})
			}
		}

		sigCh, stopSig := watchSIGWINCH()
		go func() {
			defer stopSig()
			for {
				select {
				case <-sigCh:
					resizeExec()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	outputDone := make(chan error, 1)
	go func() {
		var err error
		if tty {
			_, err = io.Copy(os.Stdout, hijacked.Reader)
		} else {
			err = copyDockerOutput(os.Stdout, os.Stderr, hijacked.Reader)
		}
		outputDone <- err
	}()

	stopInput, err := startSessionInput(os.Stdin, hijacked.Conn, hijacked.Close, hijacked.CloseWrite)
	if err != nil {
		return err
	}
	defer stopInput()
	stopInterrupt := context.AfterFunc(ctx, hijacked.Close)
	defer stopInterrupt()

	select {
	case streamErr := <-outputDone:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if streamErr != nil {
			return fmt.Errorf("reading exec output: %w", streamErr)
		}
	case <-ctx.Done():
		// Wait briefly for the output goroutine to flush remaining data
		// before terminal state is restored by the deferred RestoreTerminal.
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
		}
		return ctx.Err()
	}

	return waitDockerExecExit(ctx, cli, resp.ID)
}

// A stream close is not an exit status (e.g. a proxy disconnected). Give the
// daemon a short grace period to publish its final status, then fail closed.
func waitDockerExecExit(ctx context.Context, cli *client.Client, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for {
		inspect, err := cli.ExecInspect(ctx, id, client.ExecInspectOptions{})
		if err != nil {
			return fmt.Errorf("inspecting exec result: %w", err)
		}
		if !inspect.Running {
			if inspect.ExitCode != 0 {
				return &ExitError{Code: inspect.ExitCode}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("exec stream ended without a final exit status: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func bootstrapDockerShell(ctx context.Context, cli *client.Client, containerID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		Cmd:          []string{"/bin/sh", "-c", entrypoint.ShellBootstrapScript()},
		AttachStdout: true,
		AttachStderr: true,
		TTY:          true,
	})
	if err != nil {
		return fmt.Errorf("creating bootstrap exec: %w", err)
	}

	hijacked, err := cli.ExecAttach(ctx, resp.ID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return fmt.Errorf("attaching bootstrap exec: %w", err)
	}
	defer hijacked.Close()

	stopInterrupt := context.AfterFunc(ctx, hijacked.Close)
	defer stopInterrupt()
	var output bytes.Buffer
	_, copyErr := io.Copy(&output, io.LimitReader(hijacked.Reader, 64*1024+1))
	if copyErr != nil {
		return fmt.Errorf("reading bootstrap output: %w", copyErr)
	}

	if output.Len() > 64*1024 {
		return fmt.Errorf("bootstrap output exceeds 64 KiB")
	}
	if err := waitDockerExecExit(ctx, cli, resp.ID); err != nil {
		return fmt.Errorf("bootstrap failed: %w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

// showEntrypointOutput streams the sidecar entrypoint output (volume listing,
// warnings) to stderr. The entrypoint prints info then enters daemon mode
// (tail -f /dev/null). We follow the logs until we see a blank line marking
// the end of the entrypoint output, with a timeout as safety net.
func showEntrypointOutput(ctx context.Context, cli *client.Client, containerID string) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	reader, err := cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return
	}
	defer func() { _ = reader.Close() }()

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()
		// Empty line (possibly with \r from TTY) marks end of entrypoint output
		if strings.TrimRight(line, "\r") == "" {
			break
		}
		statusln(strings.TrimRight(line, "\r"))
	}
}
