package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/clement-tourriere/debux/internal/runtime"
)

func TestTryPatchZshCompletionAnchorsMatchCobra(t *testing.T) {
	root := NewRootCmd()
	var buf bytes.Buffer
	if err := root.GenZshCompletion(&buf); err != nil {
		t.Fatal(err)
	}
	patched, ok := tryPatchZshCompletionForSubstringMatches(buf.String())
	if !ok {
		t.Fatal("the zsh substring patch no longer matches cobra's template — update the anchors in patchZshCompletionForSubstringMatches")
	}
	if !strings.Contains(patched, "compadd -U") {
		t.Fatal("patched script should use compadd -U for substring matches")
	}
}

func TestPatchZshCompletionFallsBackOnDrift(t *testing.T) {
	stock := "#compdef debux\nsomething cobra changed entirely\n"
	if got := patchZshCompletionForSubstringMatches(stock); got != stock {
		t.Fatal("on anchor drift the stock script must be served unmodified")
	}
}

func TestCompletionCacheKeyTracksCurrentContext(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))

	kubeconfig := filepath.Join(tmp, "kubeconfig")
	writeKubeconfig(t, kubeconfig, "ctx-a")
	pathA, err := completionPodCachePath(kubeconfig, "", "default")
	if err != nil {
		t.Fatal(err)
	}

	// Same kubeconfig path, new current-context: the cache key must change,
	// otherwise the previous cluster's pods are served as fresh completions.
	writeKubeconfig(t, kubeconfig, "ctx-b")
	pathB, err := completionPodCachePath(kubeconfig, "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if pathA == pathB {
		t.Fatal("cache key must include the resolved kube-context, not just the flag value")
	}
}

func writeKubeconfig(t *testing.T, path, currentContext string) {
	t.Helper()
	content := `apiVersion: v1
kind: Config
current-context: ` + currentContext + `
contexts:
- name: ctx-a
  context: {cluster: c, user: u}
- name: ctx-b
  context: {cluster: c, user: u}
clusters:
- name: c
  cluster: {server: "https://example.invalid"}
users:
- name: u
  user: {}
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadCompletionPodCacheRejectsClockSkewAndCorruption(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(tmp, "cache"))
	kubeconfig := filepath.Join(tmp, "kubeconfig")
	writeKubeconfig(t, kubeconfig, "ctx-a")

	path, err := completionPodCachePath(kubeconfig, "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	// A future SavedAt (clock step-back) must be treated as stale, not fresh.
	future := completionPodCache{Version: completionPodCacheVersion, SavedAt: time.Now().Add(10 * time.Minute)}
	data, _ := json.Marshal(future)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCompletionPodCache(kubeconfig, "", "default"); ok {
		t.Fatal("future SavedAt must not be served as fresh")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("stale cache file should be pruned")
	}

	// Corrupt JSON: miss, and the file is removed.
	if err := os.WriteFile(path, []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := readCompletionPodCache(kubeconfig, "", "default"); ok {
		t.Fatal("corrupt cache must be a miss")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("corrupt cache file should be pruned")
	}

	// A valid fresh cache round-trips.
	if err := writeCompletionPodCache(kubeconfig, "", "default", []runtime.PodInfo{{Name: "api-1"}}, false); err != nil {
		t.Fatal(err)
	}
	cache, ok := readCompletionPodCache(kubeconfig, "", "default")
	if !ok || len(cache.Pods) != 1 || cache.Pods[0].Name != "api-1" {
		t.Fatalf("expected cache round-trip, got ok=%v cache=%+v", ok, cache)
	}
}

func TestSplitCpArg(t *testing.T) {
	tests := []struct {
		in      string
		ref     string
		path    string
		remote  bool
		wantErr bool
	}{
		{in: "./local:file", path: "./local:file"},
		{in: "/var/log/app.log", path: "/var/log/app.log"},
		{in: "plain-file", path: "plain-file"},
		{in: "my-app:/etc/nginx", ref: "my-app", path: "/etc/nginx", remote: true},
		{in: "docker://my-app:/etc", ref: "docker://my-app", path: "/etc", remote: true},
		{in: "k8s://prod/api-pod:/var/log", ref: "k8s://prod/api-pod", path: "/var/log", remote: true},
		{in: "compose://web:/usr/share", ref: "compose://web", path: "/usr/share", remote: true},
		{in: "my-app:", wantErr: true},
	}
	for _, tc := range tests {
		ref, path, remote, err := splitCpArg(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("splitCpArg(%q): expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCpArg(%q): %v", tc.in, err)
			continue
		}
		if ref != tc.ref || path != tc.path || remote != tc.remote {
			t.Errorf("splitCpArg(%q) = (%q, %q, %v), want (%q, %q, %v)", tc.in, ref, path, remote, tc.ref, tc.path, tc.remote)
		}
	}
}

func TestDowngradeUnavailableRuntime(t *testing.T) {
	checks := []runtime.DoctorCheck{
		{Name: "Docker daemon", Status: runtime.CheckFail, Detail: "daemon is not reachable"},
		{Name: "RBAC create pods", Status: runtime.CheckFail, Detail: "denied"},
	}
	out := downgradeUnavailableRuntime(checks)
	if out[0].Status != runtime.CheckWarn {
		t.Fatal("unreachable runtime should be a warning in no-target mode")
	}
	if out[1].Status != runtime.CheckFail {
		t.Fatal("non-connectivity failures must stay failures")
	}
	if checks[0].Status != runtime.CheckFail {
		t.Fatal("input slice must not be mutated")
	}
}
