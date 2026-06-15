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
