package cli

import (
	"os"
	"path/filepath"
	"strings"
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
	path := filepath.Join(t.TempDir(), "config")
	content := `apiVersion: v1
kind: Config
current-context: prod/context
clusters:
  - name: prod-cluster
    cluster:
      server: https://127.0.0.1:6443
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
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
