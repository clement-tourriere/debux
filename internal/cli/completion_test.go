package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
)

func TestCompleteKubernetesContextTargetEscapesContextForURI(t *testing.T) {
	kubeconfig := writeCompletionKubeconfig(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", kubeconfig, "")

	completions, directive := completeKubernetesTarget(cmd, "k8s://@prod")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "k8s://@prod%2Fcontext/") {
		t.Fatalf("context target completions = %#v, want escaped context URI", completions)
	}
	if directive&cobra.ShellCompDirectiveNoFileComp == 0 || directive&cobra.ShellCompDirectiveNoSpace == 0 {
		t.Fatalf("directive = %v, want no-file and no-space", directive)
	}
}

func TestCompleteKubeContextFlagUsesRawContextName(t *testing.T) {
	kubeconfig := writeCompletionKubeconfig(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", kubeconfig, "")

	completions, directive := completeKubeContextFlag(cmd, nil, "prod")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "prod/context") {
		t.Fatalf("context flag completions = %#v, want raw context name", completions)
	}
	if strings.Contains(joined, "prod%2Fcontext") {
		t.Fatalf("context flag completions should not URI-escape context names: %#v", completions)
	}
	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no-file", directive)
	}
}

func TestCompleteKubernetesRootSuggestsOnlyContexts(t *testing.T) {
	kubeconfig := writeCompletionKubeconfig(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", kubeconfig, "")

	completions, _ := completeKubernetesTarget(cmd, "k8s://")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "k8s://@prod%2Fcontext/") {
		t.Fatalf("root k8s completions = %#v, want context", completions)
	}
	if strings.Contains(joined, "k8s://gim/") || strings.Contains(joined, "pod completion unavailable") {
		t.Fatalf("root k8s completions = %#v, want contexts only", completions)
	}
}

func TestCompleteKubernetesContextPathSuggestsOnlyNamespaces(t *testing.T) {
	kubeconfig, counts := writeCompletionKubeconfigWithAPIServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", kubeconfig, "")

	completions, _ := completeKubernetesTarget(cmd, "k8s://@prod%2Fcontext/")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "k8s://@prod%2Fcontext/gim/") {
		t.Fatalf("context path completions = %#v, want namespace", completions)
	}
	if strings.Contains(joined, "api-123") || counts.pods.Load() != 0 {
		t.Fatalf("context path completions = %#v, want namespaces only (pod calls: %d)", completions, counts.pods.Load())
	}
	if counts.namespaces.Load() == 0 {
		t.Fatalf("context path completion did not list namespaces")
	}
}

func TestCompleteKubernetesNamespacePathSuggestsOnlyPods(t *testing.T) {
	kubeconfig, counts := writeCompletionKubeconfigWithAPIServer(t)
	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", kubeconfig, "")

	completions, _ := completeKubernetesTarget(cmd, "k8s://@prod%2Fcontext/gim/")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "k8s://@prod%2Fcontext/gim/api-123") {
		t.Fatalf("namespace path completions = %#v, want pod", completions)
	}
	if strings.Contains(joined, "Kubernetes namespace") || counts.namespaces.Load() != 0 {
		t.Fatalf("namespace path completions = %#v, want pods only (namespace calls: %d)", completions, counts.namespaces.Load())
	}
	if counts.pods.Load() == 0 {
		t.Fatalf("namespace path completion did not list pods")
	}
}

func TestRootPartialSchemeCompletionDoesNotForceNoSpace(t *testing.T) {
	cmd := NewRootCmd()
	completions, directive := completeRuntimeTarget(cmd, "k")
	joined := strings.Join(completions, "\n")
	if !strings.Contains(joined, "k8s://") {
		t.Fatalf("root completions = %#v, want k8s scheme", completions)
	}
	if directive&cobra.ShellCompDirectiveNoSpace != 0 {
		t.Fatalf("root directive = %v, did not want no-space because root command completions may also match", directive)
	}
}

func TestCompleteKubernetesDefaultNamespaceUsesContextNamespaceWithoutAPI(t *testing.T) {
	kubeconfig := writeCompletionKubeconfig(t)
	got := completeKubernetesDefaultNamespace(kubeconfig, "prod/context", "k8s://@prod%2Fcontext/", "k8s://@prod%2Fcontext/")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "k8s://@prod%2Fcontext/gim/") {
		t.Fatalf("default namespace completion = %#v, want context default namespace branch", got)
	}
}

func TestPodCompletionMatchesSubstringAfterThreeChars(t *testing.T) {
	if !podCompletionMatches("gim", "webapp-internal-api-abcde", "inte") {
		t.Fatalf("expected pod substring to match")
	}
	if podCompletionMatches("gim", "webapp-internal-api-abcde", "nt") {
		t.Fatalf("short substrings should not match")
	}
}

func TestKubernetesPodWorkloadNameStripsDeploymentSuffix(t *testing.T) {
	got := kubernetesPodWorkloadName("webapp-internal-api-bf55d6697-2q9nv")
	if got != "webapp-internal-api" {
		t.Fatalf("workload = %q, want webapp-internal-api", got)
	}
}

func TestZshCompletionEnablesUnfilteredDynamicMatches(t *testing.T) {
	cmd := NewRootCmd()
	var out strings.Builder
	completionCmd := newCompletionCmd(cmd)
	completionCmd.SetOut(&out)
	if err := genZshCompletionWithSubstringMatching(cmd, completionCmd); err != nil {
		t.Fatal(err)
	}
	script := out.String()
	if !strings.Contains(script, "compadd -U") {
		t.Fatalf("zsh completion should use compadd -U for pod substring matches")
	}
	if strings.Contains(script, "_describe -U") {
		t.Fatalf("zsh _describe does not support -U")
	}
}

func TestUniqueSortedCompletionsDeduplicatesByValue(t *testing.T) {
	got := uniqueSortedCompletions([]string{"img:latest\tDefault", "img:latest\tLocal image"})
	if len(got) != 1 || got[0] != "img:latest\tDefault" {
		t.Fatalf("uniqueSortedCompletions() = %#v, want first description kept once", got)
	}
}

func TestBranchAwareDirectiveNoSpaceOnlyForBranches(t *testing.T) {
	branches := branchAwareDirective([]string{"docker://\tDocker", "k8s://\tKubernetes"})
	if branches&cobra.ShellCompDirectiveNoSpace == 0 {
		t.Fatalf("branch directive = %v, want no-space", branches)
	}

	finals := branchAwareDirective([]string{"api\tDocker container"})
	if finals&cobra.ShellCompDirectiveNoSpace != 0 {
		t.Fatalf("final directive = %v, did not want no-space", finals)
	}
}

func writeCompletionKubeconfig(t *testing.T) string {
	t.Helper()
	return writeCompletionKubeconfigWithServer(t, "https://127.0.0.1:6443")
}

type completionAPICounts struct {
	namespaces atomic.Int32
	pods       atomic.Int32
}

func writeCompletionKubeconfigWithAPIServer(t *testing.T) (string, *completionAPICounts) {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	counts := &completionAPICounts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			counts.namespaces.Add(1)
			_, _ = fmt.Fprint(w, `{"apiVersion":"v1","kind":"NamespaceList","items":[{"metadata":{"name":"gim"},"status":{"phase":"Active"}},{"metadata":{"name":"prod"},"status":{"phase":"Active"}}]}`)
		case "/api/v1/namespaces/gim/pods":
			counts.pods.Add(1)
			_, _ = fmt.Fprint(w, `{"apiVersion":"meta.k8s.io/v1","kind":"PartialObjectMetadataList","items":[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"api-123","namespace":"gim"}},{"apiVersion":"v1","kind":"Pod","metadata":{"name":"web-456","namespace":"gim"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	return writeCompletionKubeconfigWithServer(t, server.URL), counts
}

func writeCompletionKubeconfigWithServer(t *testing.T, server string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: prod/context
clusters:
  - name: prod-cluster
    cluster:
      server: %s
contexts:
  - name: prod/context
    context:
      cluster: prod-cluster
      user: prod-user
      namespace: gim
  - name: dev
    context:
      cluster: prod-cluster
      user: prod-user
      namespace: default
users:
  - name: prod-user
    user: {}
`, server)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
