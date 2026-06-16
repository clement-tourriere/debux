package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/runtime"
)

func TestRecordDebugHistoryStoresResolvedKubeContext(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- name: prod-cluster
  cluster:
    server: https://127.0.0.1
contexts:
- name: prod-context
  context:
    cluster: prod-cluster
    user: prod-user
    namespace: gim
current-context: prod-context
users:
- name: prod-user
  user:
    token: test-token
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newExecCmd()
	if err := cmd.Flags().Set("kubeconfig", kubeconfig); err != nil {
		t.Fatal(err)
	}
	target := &runtime.Target{Runtime: "kubernetes", Name: "api"}
	recordDebugHistory(cmd, target, runtime.DebugOpts{Image: "debug:v1", Profile: runtime.ProfileGeneral})

	entries, err := history.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("history entries = %d, want 1", len(entries))
	}
	if entries[0].Context != "prod-context" {
		t.Fatalf("history context = %q, want prod-context", entries[0].Context)
	}
	if entries[0].Namespace != "gim" {
		t.Fatalf("history namespace = %q, want gim", entries[0].Namespace)
	}
	if entries[0].Target != "k8s://@prod-context/gim/api" {
		t.Fatalf("history target = %q", entries[0].Target)
	}
}
