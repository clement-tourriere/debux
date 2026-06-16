package cli

import (
	"strings"
	"testing"
)

func TestKillAllNamespacesConflictsWithNamespace(t *testing.T) {
	resetKillTestFlags()
	defer resetKillTestFlags()

	cmd := newKillCmd()
	cmd.SetArgs([]string{"--all-namespaces", "--namespace", "prod"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--all-namespaces cannot be combined with namespace") {
		t.Fatalf("expected namespace conflict, got %v", err)
	}
}

func TestKillAllNamespacesConflictsWithKillAll(t *testing.T) {
	resetKillTestFlags()
	defer resetKillTestFlags()

	cmd := newKillCmd()
	cmd.SetArgs([]string{"--all-namespaces", "--all"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "interactive kill picker") {
		t.Fatalf("expected --all conflict, got %v", err)
	}
}

func resetKillTestFlags() {
	flagAllNamespaces = false
	flagKillAll = false
	flagNamespace = ""
	flagKubeContext = ""
}
