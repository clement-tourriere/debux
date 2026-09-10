package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/clement-tourriere/debux/internal/history"
)

func TestKubernetesSessionScopesIncludeRecentNamespacesForContext(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	kubeconfig := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, kubeconfig, "ctx-a")
	t.Setenv("KUBECONFIG", kubeconfig)

	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "ctx-a", Namespace: "gim", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "ctx-b", Namespace: "prod", Name: "api"}); err != nil {
		t.Fatal(err)
	}

	scopes := kubernetesSessionScopes("", "ctx-a", "", true)
	if !hasKubernetesSessionScope(scopes, "ctx-a", "gim") {
		t.Fatalf("scopes = %#v, want recent ctx-a/gim", scopes)
	}
	if hasKubernetesSessionScope(scopes, "ctx-b", "prod") {
		t.Fatalf("scopes = %#v, did not filter by requested context", scopes)
	}
}

func TestKubernetesSessionScopesSkipRemovedHistoryContexts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "config")
	writeKubeconfig(t, kubeconfig, "ctx-a")
	for _, entry := range []history.Entry{
		{Runtime: "kubernetes", Context: "ctx-a", Namespace: "gim"},
		{Runtime: "kubernetes", Context: "ctx-b", Namespace: "prod"},
		{Runtime: "kubernetes", Context: "ctx-b", Namespace: "prod"}, // duplicate
		{Runtime: "kubernetes", Namespace: "legacy"},                 // current-context fallback
		{Runtime: "docker", Context: "ctx-a", Namespace: "ignored"},
	} {
		if err := history.Append(entry); err != nil {
			t.Fatal(err)
		}
	}
	// Stale entries must not consume the scope budget and hide valid older ones.
	for i := range maxDefaultKubernetesSessionScopes {
		if err := history.Append(history.Entry{Runtime: "kubernetes", Context: fmt.Sprintf("kind-deleted-%d", i), Namespace: "debux-e2e"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit-kubeconfig=%t", explicit), func(t *testing.T) {
			path := ""
			t.Setenv("KUBECONFIG", kubeconfig)
			if explicit {
				path = kubeconfig
				// --kubeconfig must take precedence over KUBECONFIG.
				t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
			}
			scopes := kubernetesSessionScopes(path, "", "", true)
			want := []kubernetesSessionScope{{namespace: "default"}, {context: "ctx-a", namespace: "gim"}, {context: "ctx-b", namespace: "prod"}, {namespace: "legacy"}}
			if len(scopes) != len(want) {
				t.Fatalf("scopes = %#v, want only configured or contextless scopes: %#v", scopes, want)
			}
			for _, scope := range want {
				if !hasKubernetesSessionScope(scopes, scope.context, scope.namespace) {
					t.Errorf("scopes = %#v, missing %#v", scopes, scope)
				}
			}
		})
	}
	entries, err := history.Load()
	if err != nil || len(entries) != maxDefaultKubernetesSessionScopes+5 {
		t.Fatalf("discovery must not modify history: entries=%#v, err=%v", entries, err)
	}
}

func TestKubernetesSessionScopesPreserveRequestedScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "config")
	// Even a missing current-context must still be queried so its error surfaces.
	writeKubeconfig(t, kubeconfig, "missing")
	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "ctx-a", Namespace: "history"}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name           string
		context        string
		namespace      string
		includeHistory bool
	}{
		{name: "explicit missing context", context: "missing", includeHistory: true},
		{name: "explicit namespace", namespace: "gim", includeHistory: true},
		{name: "explicit missing context and namespace", context: "missing", namespace: "gim", includeHistory: true},
		{name: "no history"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scopes := kubernetesSessionScopes(kubeconfig, tt.context, tt.namespace, tt.includeHistory)
			ns := tt.namespace
			if ns == "" {
				ns = "default"
			}
			if len(scopes) != 1 || scopes[0].context != tt.context || scopes[0].namespace != ns {
				t.Fatalf("scopes = %#v, want requested scope only", scopes)
			}
		})
	}
}

func TestKubernetesSessionScopesInvalidKubeconfigDoesNotExpandHistory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	kubeconfig := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(kubeconfig, []byte("invalid: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "old", Namespace: "gim"}); err != nil {
		t.Fatal(err)
	}
	scopes := kubernetesSessionScopes(kubeconfig, "", "", true)
	if len(scopes) != 1 || scopes[0].context != "" || scopes[0].namespace != "default" {
		t.Fatalf("scopes = %#v, want only current scope to report kubeconfig error", scopes)
	}
}

func TestCollectDebugSessionsSkipsRemovedContextsButReportsClusterErrors(t *testing.T) {
	for _, forbidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("forbidden=%t", forbidden), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if forbidden {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, `{"apiVersion":"v1","kind":"PodList","items":[]}`)
			}))
			t.Cleanup(server.Close)
			kubeconfig := writeCompletionKubeconfigWithServer(t, server.URL)
			for _, entry := range []history.Entry{
				{Runtime: "kubernetes", Context: "dev", Namespace: "recent"},
				{Runtime: "kubernetes", Context: "kind-deleted", Namespace: "debux-e2e"},
			} {
				if err := history.Append(entry); err != nil {
					t.Fatal(err)
				}
			}
			cmd := newListCmd()
			if err := cmd.Flags().Set("kubeconfig", kubeconfig); err != nil {
				t.Fatal(err)
			}
			_, problems := collectDebugSessions(t.Context(), cmd, "kubernetes", nil, "", "", false)
			wantProblems := 0
			if forbidden {
				wantProblems = 2 // Current scope and a configured history scope.
			}
			if len(problems) != wantProblems {
				t.Fatalf("problems = %v, want %d cluster errors only", problems, wantProblems)
			}
			for _, problem := range problems {
				if !strings.Contains(problem.Error(), "listing pods:") {
					t.Errorf("unexpected error: %v", problem)
				}
			}
			_, problems = collectDebugSessions(t.Context(), cmd, "kubernetes", nil, "kind-deleted", "", false)
			if len(problems) != 1 || !strings.Contains(problems[0].Error(), `context "kind-deleted" does not exist`) {
				t.Fatalf("explicit missing context error must remain visible, got %v", problems)
			}
		})
	}
}

func hasKubernetesSessionScope(scopes []kubernetesSessionScope, context, namespace string) bool {
	for _, scope := range scopes {
		if scope.context == context && scope.namespace == namespace {
			return true
		}
	}
	return false
}
