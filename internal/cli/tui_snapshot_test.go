package cli

import (
	"github.com/spf13/cobra"
)

type flagState struct {
	value   string
	changed bool
}

func snapshotFlagState(cmd *cobra.Command, names ...string) map[string]flagState {
	snap := make(map[string]flagState, len(names))
	for _, name := range names {
		if f := cmd.Flags().Lookup(name); f != nil {
			snap[name] = flagState{value: f.Value.String(), changed: f.Changed}
		}
	}
	return snap
}

// restoreFlagState restores values only within this command's flag set.
func restoreFlagState(cmd *cobra.Command, snap map[string]flagState) {
	for name, s := range snap {
		if f := cmd.Flags().Lookup(name); f != nil {
			_ = f.Value.Set(s.value)
			f.Changed = s.changed
		}
	}
}
