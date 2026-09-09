package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/moby/moby/api/types/container"
	"k8s.io/client-go/tools/remotecommand"
)

func TestKillDebugSessionUsesSelectedImmutableDockerID(t *testing.T) {
	var removed atomic.Bool
	info := container.InspectResponse{ID: "second-id", Name: "/debux-second", State: &container.State{Running: true}, Config: &container.Config{Labels: map[string]string{dockerLabelManagedBy: dockerLabelManagedByVal, dockerLabelKind: dockerLabelKindSidecar, dockerLabelTargetName: "same-app"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1.55/containers/second-id/json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(info)
		case "DELETE /v1.55/containers/second-id":
			removed.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("re-resolved selected session instead of using its ID: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.55")
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	selected := DebugSessionInfo{Runtime: "docker", Target: "docker://same-app", DebugName: "debux-second", ID: "second-id"}
	if err := KillDebugSession(t.Context(), selected, ""); err != nil {
		t.Fatal(err)
	}
	if !removed.Load() {
		t.Fatal("selected container was not removed")
	}
}

type finishedKubernetesExecutor struct{}

func (finishedKubernetesExecutor) Stream(remotecommand.StreamOptions) error { return nil }
func (finishedKubernetesExecutor) StreamWithContext(context.Context, remotecommand.StreamOptions) error {
	return nil
}

func TestKubernetesStreamReturnsStdinOwnership(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	for range 8 {
		if err := streamKubernetesSession(t.Context(), finishedKubernetesExecutor{}, remotecommand.StreamOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = writer.Write([]byte("next-TUI-key"))
	_ = writer.Close()
	got, err := io.ReadAll(input)
	if err != nil || string(got) != "next-TUI-key" {
		t.Fatalf("Kubernetes stream consumed future input: %q %v", got, err)
	}
}

func TestSessionMetadataEnvironmentIsReserved(t *testing.T) {
	for _, value := range []string{"DEBUX_OPTIONS_HASH=fake", "DEBUX_SECURITY_PROFILE=general", "DEBUX_TARGET_ROOT=/proc/1/root", "TOKEN=bad\x00value"} {
		if _, err := debugExtraEnv([]string{value}, nil); err == nil {
			t.Fatalf("unsafe environment accepted: %q", value)
		}
	}
}
