package cli

import (
	"testing"

	"github.com/clement-tourriere/debux/internal/config"
)

// TestApplySavedTUIStateKeepsVolumeDefaultWithoutState locks the regression
// where a fresh install (no saved state) flipped the volume-sharing default
// from on to off: applySavedTUIState must not overwrite flag-derived defaults
// when there is nothing persisted to restore.
func TestApplySavedTUIStateKeepsVolumeDefaultWithoutState(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir()) // empty cache dir => no saved state

	cmd := newProfileTestCmd(t)
	launch := tuiLaunch{shareVolumes: true}
	applySavedTUIState(cmd, &launch)

	if !launch.shareVolumes {
		t.Fatal("applySavedTUIState disabled volume sharing despite no saved state")
	}
}

// TestApplySavedTUIStateAppliesSavedValues confirms that once a state file
// exists, its values (including a deliberately disabled shareVolumes) are
// restored, so the no-state guard does not break the round-trip.
func TestApplySavedTUIStateAppliesSavedValues(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if err := config.SaveTUIState(config.TUIState{
		Image:     "saved:img",
		NoVolumes: true, // user turned volumes off on a previous launch
	}); err != nil {
		t.Fatalf("SaveTUIState: %v", err)
	}

	cmd := newProfileTestCmd(t)
	launch := tuiLaunch{image: "default:img", shareVolumes: true}
	applySavedTUIState(cmd, &launch)

	if launch.image != "saved:img" {
		t.Errorf("image = %q, want %q", launch.image, "saved:img")
	}
	if launch.shareVolumes {
		t.Error("shareVolumes should reflect the saved (off) value once a state exists")
	}
}

// TestTUILaunchStateDiffOmitsDefaults locks the regression where the TUI
// persisted the fully resolved image/profile, pinning an old release's
// version-tagged default image (and overriding config edits) forever.
func TestTUILaunchStateDiffOmitsDefaults(t *testing.T) {
	defaults := tuiLaunch{
		image:        "ghcr.io/x/debux:0.8.3",
		profile:      "restricted",
		pullPolicy:   "",
		shareVolumes: true,
	}

	same := tuiLaunchStateDiff(defaults, defaults)
	if same.Image != "" || same.Profile != "" || same.User != "" || same.PullPolicy != "" {
		t.Fatalf("unchanged launch persisted resolved defaults: %+v", same)
	}

	changed := defaults
	changed.image = "ghcr.io/x/custom:1"
	changed.profile = "sysadmin"
	diff := tuiLaunchStateDiff(changed, defaults)
	if diff.Image != "ghcr.io/x/custom:1" {
		t.Errorf("Image = %q, want the explicit override", diff.Image)
	}
	if diff.Profile != "sysadmin" {
		t.Errorf("Profile = %q, want the explicit override", diff.Profile)
	}
}

// TestRestoreFlagStateResetsChanged locks the regression where launching a
// Kubernetes target from the TUI left --profile marked as changed, so the
// next Docker pick failed validateExecFlags and aborted the whole TUI.
func TestRestoreFlagStateResetsChanged(t *testing.T) {
	cmd := newProfileTestCmd(t)
	snap := snapshotFlagState(cmd, "profile", "namespace")

	// Simulate a Kubernetes launch marking flags as changed.
	if err := cmd.Flags().Set("profile", "sysadmin"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("namespace", "prod"); err != nil {
		t.Fatal(err)
	}
	if err := validateExecFlags(cmd, "docker"); err == nil {
		t.Fatal("expected changed kube-only flags to fail docker validation")
	}

	restoreFlagState(cmd, snap)
	if err := validateExecFlags(cmd, "docker"); err != nil {
		t.Fatalf("restored flags should pass docker validation, got %v", err)
	}
	if got, _ := cmd.Flags().GetString("profile"); got != "general" {
		t.Fatalf("profile value = %q, want restored default", got)
	}
}
