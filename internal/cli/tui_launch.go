package cli

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/clement-tourriere/debux/internal/config"
	"github.com/spf13/cobra"
)

func runTUI(cmd *cobra.Command, _ []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	// resolveProfile falls back to the config file's default profile when no
	// flag was passed, matching plain `debux exec`.
	profile, err := resolveProfile(cmd)
	if err != nil {
		return err
	}
	pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagString(cmd, "pull-policy")))
	if err != nil {
		return err
	}
	image := resolveImage(flagString(cmd, "image"))
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")

	baseLaunch := tuiLaunch{
		image:           image,
		user:            flagString(cmd, "user"),
		pullPolicy:      pullPolicy,
		profile:         profile,
		fresh:           flagBool(cmd, "fresh"),
		copy:            flagBool(cmd, "copy"),
		privileged:      flagBool(cmd, "privileged"),
		shareVolumes:    !flagBool(cmd, "no-volumes"),
		readOnlyVolumes: flagBool(cmd, "read-only-volumes"),
	}

	// Snapshot the flag/config-resolved defaults before layering saved TUI
	// state on top: only deviations from these defaults are persisted, so a
	// config edit or a release changing the default image is not masked by
	// state saved in an older session.
	defaultLaunch := baseLaunch

	// Restore last-used TUI options when the user did not explicitly override
	// them on the command line.
	applySavedTUIState(cmd, &baseLaunch)

	// Carry the navigation scope and any launch error across loop iterations
	// so the next TUI reopens where the user left off and shows what failed.
	kubeCtx, kubeNs := flagString(cmd, "context"), flagString(cmd, "namespace")
	startupNotice := ""
	for {
		launch := baseLaunch
		m := newTUIModel(&launch, kubeconfig, kubeCtx, kubeNs)
		m.notice = startupNotice
		startupNotice = ""
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
		finalModel, err := p.Run()
		if err != nil {
			// SIGTERM/SIGHUP cancel the signal context; bubbletea has already
			// restored the terminal, so exit quietly like Ctrl-C does.
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if fm, ok := finalModel.(tuiModel); ok {
			kubeCtx, kubeNs = fm.selectedContext, fm.selectedNamespace
		}
		if launch.target == "" {
			return nil
		}
		if launch.session != nil {
			prepareTerminalForDebugSession()
			var err error
			if launch.mode == tuiLaunchTerminal {
				err = openInTerminal(ctx, cmd, launch)
			} else {
				err = attachDebugSession(ctx, cmd, *launch.session)
			}
			if err != nil {
				startupNotice = "Session ended: " + err.Error()
			}
			continue
		}

		baseLaunch = launch
		baseLaunch.target = ""
		baseLaunch.mode = ""

		prepareTerminalForDebugSession()

		// Persist the options used for this launch so the next `debux tui`
		// invocation can start with the same defaults.
		_ = config.SaveTUIState(tuiLaunchStateDiff(launch, defaultLaunch))

		if launch.mode == tuiLaunchTerminal {
			if err := openInTerminal(ctx, cmd, launch); err != nil {
				startupNotice = "External launch failed: " + err.Error()
			}
			continue
		}
		// Each launch owns a new command tree. No mutable flags leak back
		// into the dashboard or the next Docker/Kubernetes session.
		child := NewRootCmd()
		child.SetArgs(buildExecArgs(cmd, launch))
		if err := child.ExecuteContext(ctx); err != nil {
			startupNotice = "Session ended: " + err.Error()
		}
	}
}

func applySavedTUIState(cmd *cobra.Command, launch *tuiLaunch) {
	state := config.LoadTUIState()
	if !flagChanged(cmd, "image") && state.Image != "" {
		launch.image = state.Image
	}
	if !flagChanged(cmd, "user") && state.User != "" {
		launch.user = state.User
	}
	if !flagChanged(cmd, "pull-policy") && state.PullPolicy != "" {
		launch.pullPolicy = state.PullPolicy
	}
	if !flagChanged(cmd, "profile") && !flagChanged(cmd, "privileged") && state.Profile != "" {
		launch.profile = state.Profile
		launch.privileged = state.Privileged
	}
	if !flagChanged(cmd, "fresh") {
		launch.fresh = state.Fresh
	}
	if !flagChanged(cmd, "copy") {
		launch.copy = state.Copy
	}
	if !flagChanged(cmd, "no-volumes") && !flagChanged(cmd, "read-only-volumes") {
		// NoVolumes is stored inverted so an absent/zero state keeps the
		// default of sharing volumes rather than flipping it off.
		launch.shareVolumes = !state.NoVolumes
		launch.readOnlyVolumes = state.ReadOnlyVolumes
	}
}

// tuiLaunchStateDiff persists only the options that deviate from the
// flag/config-resolved defaults. Values equal to the defaults are stored as
// zero values ("follow the defaults"), so an upgraded release's new default
// image or an edited config file takes effect instead of being pinned by
// state saved in an older session.
func tuiLaunchStateDiff(launch, defaults tuiLaunch) config.TUIState {
	state := config.TUIState{
		Fresh:           launch.fresh,
		Copy:            launch.copy,
		NoVolumes:       !launch.shareVolumes,
		ReadOnlyVolumes: launch.readOnlyVolumes,
	}
	if launch.image != defaults.image {
		state.Image = launch.image
	}
	if launch.user != defaults.user {
		state.User = launch.user
	}
	if launch.pullPolicy != defaults.pullPolicy {
		state.PullPolicy = launch.pullPolicy
	}
	if launch.profile != defaults.profile || launch.privileged != defaults.privileged {
		state.Profile = launch.profile
		state.Privileged = launch.privileged
	}
	return state
}
