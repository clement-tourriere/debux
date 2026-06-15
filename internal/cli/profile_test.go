package cli

import (
	"strings"
	"testing"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newProfileTestCmd(t *testing.T) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test"}
	addExecFlags(cmd)
	return cmd
}

func TestResolveProfileDefaults(t *testing.T) {
	cmd := newProfileTestCmd(t)

	got, err := resolveProfile(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != runtime.ProfileGeneral {
		t.Errorf("default profile = %q, want %q", got, runtime.ProfileGeneral)
	}
}

func TestResolveProfileExplicit(t *testing.T) {
	for _, want := range runtime.ValidProfiles {
		cmd := newProfileTestCmd(t)
		if err := cmd.Flags().Set("profile", want); err != nil {
			t.Fatalf("set profile: %v", err)
		}
		got, err := resolveProfile(cmd)
		if err != nil {
			t.Fatalf("resolveProfile(%q): %v", want, err)
		}
		if got != want {
			t.Errorf("resolveProfile() = %q, want %q", got, want)
		}
	}
}

func TestResolveProfilePrivileged(t *testing.T) {
	cmd := newProfileTestCmd(t)

	if err := cmd.Flags().Set("privileged", "true"); err != nil {
		t.Fatalf("set privileged: %v", err)
	}
	got, err := resolveProfile(cmd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != runtime.ProfileSysadmin {
		t.Errorf("privileged profile = %q, want %q", got, runtime.ProfileSysadmin)
	}
}

func TestResolveProfileConflict(t *testing.T) {
	cmd := newProfileTestCmd(t)

	if err := cmd.Flags().Set("privileged", "true"); err != nil {
		t.Fatalf("set privileged: %v", err)
	}
	if err := cmd.Flags().Set("profile", runtime.ProfileRestricted); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	if _, err := resolveProfile(cmd); err == nil {
		t.Fatal("expected conflict error for --privileged with --profile=restricted")
	} else if !strings.Contains(err.Error(), "conflicting flags") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveProfileInvalid(t *testing.T) {
	cmd := newProfileTestCmd(t)

	if err := cmd.Flags().Set("profile", "not-a-profile"); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	if _, err := resolveProfile(cmd); err == nil {
		t.Fatal("expected error for invalid profile")
	}
}
