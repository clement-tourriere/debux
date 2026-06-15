package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/clement-tourriere/debux/internal/entrypoint"
	"github.com/moby/term"

	corev1 "k8s.io/api/core/v1"
	remotecommandconsts "k8s.io/apimachinery/pkg/util/remotecommand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
	"k8s.io/streaming/pkg/httpstream"
)

func newRemoteExecutor(config *rest.Config, reqURL *url.URL) (remotecommand.Executor, error) {
	spdyExec, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, reqURL)
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	websocketExec, err := remotecommand.NewWebSocketExecutorForProtocols(
		config,
		http.MethodGet,
		reqURL.String(),
		remotecommandconsts.StreamProtocolV5Name,
		remotecommandconsts.StreamProtocolV4Name,
		remotecommandconsts.StreamProtocolV3Name,
		remotecommandconsts.StreamProtocolV2Name,
	)
	if err != nil {
		return nil, fmt.Errorf("creating websocket executor: %w", err)
	}

	exec, err := remotecommand.NewFallbackExecutor(spdyExec, websocketExec, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return nil, fmt.Errorf("creating fallback executor: %w", err)
	}

	return exec, nil
}

// KubernetesExec debugs a running pod using ephemeral containers.
// It reuses an existing running debux container when possible, or creates a new
// one in daemon mode (DEBUX_DAEMON=1) so it stays alive between sessions.

func execInPodWithMetadata(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName, targetLabel, kubeContext string, command []string) error {
	if err := bootstrapPodShell(ctx, config, clientset, namespace, podName, containerName); err != nil {
		return fmt.Errorf("preparing debux shell config: %w", err)
	}
	cmd := debuxExecCommand(command)
	cmd[2] = "export DEBUX_TARGET=" + shellQuote(targetLabel) + " DEBUX_CONTEXT=" + shellQuote(kubeContext) + "; " + cmd[2]
	return execInPodWithCommand(ctx, config, clientset, namespace, podName, containerName, cmd)
}

func bootstrapPodShell(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   []string{"/bin/sh"},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating bootstrap executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  strings.NewReader(entrypoint.ShellBootstrapScript() + "\n"),
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		details := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if details != "" {
			return fmt.Errorf("running bootstrap: %w: %s", err, details)
		}
		return fmt.Errorf("running bootstrap: %w", err)
	}
	return nil
}

// kubernetesExecError converts the remote command's exit status into the
// typed ExitError so the CLI propagates the real code instead of printing a
// spurious "command terminated with exit code N".

func kubernetesExecError(err error) error {
	if err == nil {
		return nil
	}
	var codeErr k8sexec.CodeExitError
	if errors.As(err, &codeErr) {
		return &ExitError{Code: codeErr.Code}
	}
	return err
}

func execInPodWithCommand(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, command []string) error {
	// Allocate a remote TTY only when stdio is a terminal, mirroring kubectl:
	// piped one-shot commands must not get CRLF-mangled, echo-polluted output.
	tty := stdioIsTTY()

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   command,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty, // TTY merges stderr into stdout
			TTY:       tty,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Tty:    tty,
	}

	if tty {
		stdinFd, _ := term.GetFdInfo(os.Stdin)
		oldState, rawErr := term.SetRawTerminal(stdinFd)
		if rawErr == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
		streamOpts.Stderr = &bytes.Buffer{}
	} else {
		streamOpts.Stderr = os.Stderr
	}

	return kubernetesExecError(exec.StreamWithContext(ctx, streamOpts))
}

// KubernetesPod creates a standalone debug pod.

func attachToPod(ctx context.Context, config *rest.Config, clientset *kubernetes.Clientset, namespace, podName, containerName string, tty bool) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: containerName,
			Stdin:     true,
			Stdout:    true,
			Stderr:    !tty, // TTY merges stderr into stdout
			TTY:       tty,
		}, scheme.ParameterCodec)

	exec, err := newRemoteExecutor(config, req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	streamOpts := remotecommand.StreamOptions{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Tty:    tty,
	}

	if tty {
		stdinFd, _ := term.GetFdInfo(os.Stdin)
		oldState, rawErr := term.SetRawTerminal(stdinFd)
		if rawErr == nil {
			defer func() {
				_ = term.RestoreTerminal(stdinFd, oldState)
				resetTerminalEmulator()
			}()
		}
		tsq := newTerminalSizeQueue(stdinFd)
		defer tsq.Close()
		streamOpts.TerminalSizeQueue = tsq
		streamOpts.Stderr = &bytes.Buffer{}
	} else {
		streamOpts.Stderr = os.Stderr
	}

	return kubernetesExecError(exec.StreamWithContext(ctx, streamOpts))
}

type terminalSizeQueue struct {
	resizeChan   chan remotecommand.TerminalSize
	stopResizing chan struct{}
	done         chan struct{}
	stopOnce     sync.Once
}

func parsePositiveUint16Env(key string) (uint16, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(v, 10, 16)
	if err != nil || n == 0 {
		return 0, false
	}
	return uint16(n), true
}

func terminalSizeFromFd(fd uintptr) (remotecommand.TerminalSize, bool) {
	ws, err := term.GetWinsize(fd)
	if err != nil || ws == nil || ws.Width == 0 || ws.Height == 0 {
		return remotecommand.TerminalSize{}, false
	}
	return remotecommand.TerminalSize{Width: ws.Width, Height: ws.Height}, true
}

func detectTerminalSize(fd uintptr) remotecommand.TerminalSize {
	if size, ok := terminalSizeFromFd(fd); ok {
		return size
	}

	stdoutFd, stdoutIsTerminal := term.GetFdInfo(os.Stdout)
	if stdoutIsTerminal {
		if size, ok := terminalSizeFromFd(stdoutFd); ok {
			return size
		}
	}

	if cols, okCols := parsePositiveUint16Env("COLUMNS"); okCols {
		if lines, okLines := parsePositiveUint16Env("LINES"); okLines {
			return remotecommand.TerminalSize{Width: cols, Height: lines}
		}
	}

	return remotecommand.TerminalSize{Width: 80, Height: 24}
}

func newTerminalSizeQueue(fd uintptr) *terminalSizeQueue {
	tsq := &terminalSizeQueue{
		resizeChan:   make(chan remotecommand.TerminalSize, 1),
		stopResizing: make(chan struct{}),
		done:         make(chan struct{}),
	}

	tsq.resizeChan <- detectTerminalSize(fd)

	go tsq.monitorSize(fd)

	return tsq
}
