package cli

import (
	"path/filepath"
	"testing"

	"github.com/clement-tourriere/debux/internal/history"
)

func TestKubernetesSessionScopesIncludeRecentNamespacesForContext(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))

	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "eks-preprod-01", Namespace: "gim", Name: "api"}); err != nil {
		t.Fatal(err)
	}
	if err := history.Append(history.Entry{Runtime: "kubernetes", Context: "other", Namespace: "prod", Name: "api"}); err != nil {
		t.Fatal(err)
	}

	scopes := kubernetesSessionScopes("", "eks-preprod-01", "", true)
	if !hasKubernetesSessionScope(scopes, "eks-preprod-01", "gim") {
		t.Fatalf("scopes = %#v, want recent eks-preprod-01/gim", scopes)
	}
	if hasKubernetesSessionScope(scopes, "other", "prod") {
		t.Fatalf("scopes = %#v, did not filter by requested context", scopes)
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
