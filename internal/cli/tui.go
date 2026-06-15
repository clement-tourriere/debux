package cli

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/clement-tourriere/debux/internal/config"
	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func newTUICmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tui",
		Aliases: []string{"ui"},
		Short:   "Open the full-screen debux target browser",
		Long: `Open an interactive full-screen TUI to search Docker containers,
Kubernetes contexts/namespaces/pods, active debux sessions, and recent history.

Kubernetes pods are loaded lazily after you choose a context and namespace, so
large namespaces do not block startup. Press enter to open the selected debug
shell in the current terminal; when the shell exits, debux returns to the TUI.
External terminal launch is disabled unless DEBUX_TERMINAL is configured.`,
		RunE: runTUI,
	}
	addExecFlags(cmd)
	return cmd
}

type tuiLaunchMode string

const (
	tuiLaunchCurrent  tuiLaunchMode = "current"
	tuiLaunchTerminal tuiLaunchMode = "terminal"
)

type tuiLaunch struct {
	target          string
	mode            tuiLaunchMode
	image           string
	user            string
	pullPolicy      string
	profile         string
	fresh           bool
	copy            bool
	privileged      bool
	shareVolumes    bool
	readOnlyVolumes bool
}

type tuiView string

const (
	tuiViewDashboard      tuiView = "dashboard"
	tuiViewDocker         tuiView = "docker"
	tuiViewKubeContexts   tuiView = "kubernetes-contexts"
	tuiViewKubeNamespaces tuiView = "kubernetes-namespaces"
	tuiViewKubePods       tuiView = "kubernetes-pods"
	tuiViewSessions       tuiView = "sessions"
	tuiViewHistory        tuiView = "history"
)

type tuiSource string

const (
	tuiSourceDocker   tuiSource = "docker"
	tuiSourceK8s      tuiSource = "kubernetes"
	tuiSourceSessions tuiSource = "sessions"
	tuiSourceHistory  tuiSource = "history"
)

type tuiItemKind string

const (
	tuiItemSource        tuiItemKind = "source"
	tuiItemTarget        tuiItemKind = "target"
	tuiItemKubeContext   tuiItemKind = "kube-context"
	tuiItemKubeNamespace tuiItemKind = "kube-namespace"
)

type tuiItem struct {
	kind          tuiItemKind
	source        tuiSource
	view          tuiView
	title         string
	desc          string
	target        string
	context       string
	namespace     string
	launchImage   string
	launchUser    string
	launchProfile string
	active        bool
}

func (i tuiItem) Title() string { return i.title }

func (i tuiItem) Description() string { return i.desc }

func (i tuiItem) FilterValue() string {
	// Filter on identifying fields only. Including kind/source/desc makes
	// generic words ("docker", "pod", "kubernetes") match every row.
	return strings.Join([]string{i.title, i.target, i.context, i.namespace}, " ")
}

type tuiDashboardLoadedMsg struct {
	gen        int
	docker     []tuiItem
	contexts   []tuiItem
	history    []tuiItem
	dockerErr  error
	contextErr error
	historyErr error
	loadedAt   time.Time
}

type tuiNamespacesLoadedMsg struct {
	gen        int
	context    string
	namespaces []tuiItem
	err        error
}

type tuiPodsLoadedMsg struct {
	gen       int
	context   string
	namespace string
	query     string
	pods      []tuiItem
	limited   bool
	err       error
}

type tuiSessionsLoadedMsg struct {
	gen      int
	sessions []tuiItem
	err      error
	loadedAt time.Time
}

type tuiModel struct {
	list   list.Model
	view   tuiView
	width  int
	height int
	result *tuiLaunch

	// loadGen sequences async loads: a result whose generation does not match
	// is from an abandoned request and must not overwrite newer state.
	loadGen int
	// lastAppliedView tracks the view the list was last populated for, so the
	// cursor and filter reset on view switches but survive in-view reloads.
	lastAppliedView tuiView

	dockerItems    []tuiItem
	contextItems   []tuiItem
	namespaceItems []tuiItem
	podItems       []tuiItem
	sessionItems   []tuiItem
	historyItems   []tuiItem

	loading        bool
	loadingLabel   string
	notice         string
	lastLoadedAt   time.Time
	dockerErr      error
	contextErr     error
	namespaceErr   error
	podsErr        error
	sessionErr     error
	historyErr     error
	podsLimited    bool
	sessionsLoaded bool

	selectedContext   string
	selectedNamespace string
	podQuery          string
	searchingPods     bool
	podSearch         textinput.Model

	image           string
	user            string
	pullPolicy      string
	profile         string
	fresh           bool
	copy            bool
	privileged      bool
	shareVolumes    bool
	readOnlyVolumes bool
	kubeconfig      string
}

// The palette adapts to the terminal background so text stays readable on
// light themes, not only dark ones.
var (
	tuiAccent         = lipgloss.AdaptiveColor{Light: "#7C3AED", Dark: "#8B5CF6"}
	tuiAccent2        = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#06B6D4"}
	tuiMuted          = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#7C7C86"}
	tuiText           = lipgloss.AdaptiveColor{Light: "#1F2937", Dark: "#E5E7EB"}
	tuiSelectedText   = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#FFFFFF"}
	tuiSelectedDesc   = lipgloss.AdaptiveColor{Light: "#6D28D9", Dark: "#C4B5FD"}
	tuiSelectedBg     = lipgloss.AdaptiveColor{Light: "#EDE9FE", Dark: "#1E1B2E"}
	tuiActiveTitle    = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FDE68A"}
	tuiPanelBorder    = lipgloss.AdaptiveColor{Light: "#D1D5DB", Dark: "#2D2D35"}
	tuiPillBackground = lipgloss.AdaptiveColor{Light: "#E5E7EB", Dark: "#27272A"}
	tuiTitleStyle     = lipgloss.NewStyle().Bold(true).Foreground(tuiText)
	tuiLogoStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(tuiAccent).Padding(0, 1)
	tuiHeaderStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(tuiAccent).Padding(0, 1)
	tuiTabStyle       = lipgloss.NewStyle().Padding(0, 1).Foreground(tuiMuted)
	tuiActiveTab      = tuiTabStyle.Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(tuiAccent)
	tuiHintStyle      = lipgloss.NewStyle().Foreground(tuiMuted)
	tuiWarnStyle      = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#F59E0B"})
	tuiSuccessStyle   = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#22C55E"})
	tuiPanelStyle     = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(tuiPanelBorder).Padding(0, 1)
	tuiPillStyle      = lipgloss.NewStyle().Padding(0, 1).Foreground(tuiText).Background(tuiPillBackground)
	tuiPillOnStyle    = tuiPillStyle.Foreground(lipgloss.Color("#FFFFFF")).Background(tuiAccent)
)

func runTUI(cmd *cobra.Command, _ []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	profile := runtime.ProfileGeneral
	var err error
	if flagChanged(cmd, "profile") || flagChanged(cmd, "privileged") {
		profile, err = resolveProfile(cmd)
		if err != nil {
			return err
		}
	}
	pullPolicy, err := resolvePullPolicy(configuredPullPolicy(flagPullPolicy))
	if err != nil {
		return err
	}
	image := resolveImage(flagImage)
	kubeconfig, _ := cmd.Flags().GetString("kubeconfig")

	baseLaunch := tuiLaunch{
		image:           image,
		user:            flagUser,
		pullPolicy:      pullPolicy,
		profile:         profile,
		fresh:           flagFresh,
		copy:            flagCopy,
		privileged:      flagPrivileged,
		shareVolumes:    !flagNoVolumes,
		readOnlyVolumes: flagReadOnlyVolumes,
	}

	// Restore last-used TUI options when the user did not explicitly override
	// them on the command line.
	applySavedTUIState(cmd, &baseLaunch)

	// Carry the navigation scope and any launch error across loop iterations
	// so the next TUI reopens where the user left off and shows what failed.
	kubeCtx, kubeNs := flagKubeContext, flagNamespace
	startupNotice := ""
	for {
		launch := baseLaunch
		m := newTUIModel(&launch, kubeconfig, kubeCtx, kubeNs)
		m.notice = startupNotice
		startupNotice = ""
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
		finalModel, err := p.Run()
		if err != nil {
			return err
		}
		if fm, ok := finalModel.(tuiModel); ok {
			kubeCtx, kubeNs = fm.selectedContext, fm.selectedNamespace
		}
		if launch.target == "" {
			return nil
		}

		flagImage = launch.image
		flagUser = launch.user
		flagPullPolicy = launch.pullPolicy
		flagProfile = launch.profile
		flagFresh = launch.fresh
		flagCopy = launch.copy
		flagPrivileged = launch.privileged
		flagNoVolumes = !launch.shareVolumes
		flagReadOnlyVolumes = launch.readOnlyVolumes
		// resolveProfile honors --profile only when cobra marks it as changed;
		// without this, a profile toggled in the TUI silently launches the
		// default profile.
		if strings.HasPrefix(launch.target, "k8s://") && launch.profile != "" && !launch.privileged {
			_ = cmd.Flags().Set("profile", launch.profile)
		}
		baseLaunch = launch
		baseLaunch.target = ""
		baseLaunch.mode = ""

		prepareTerminalForDebugSession()

		// Persist the options used for this launch so the next `debux tui`
		// invocation can start with the same defaults.
		_ = config.SaveTUIState(tuiLaunchToState(launch))

		if launch.mode == tuiLaunchTerminal {
			if err := openInTerminal(ctx, cmd, launch); err != nil {
				startupNotice = "External launch failed: " + err.Error()
			}
			continue
		}
		if err := runExec(cmd, []string{launch.target}); err != nil {
			return err
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

func tuiLaunchToState(launch tuiLaunch) config.TUIState {
	return config.TUIState{
		Image:           launch.image,
		User:            launch.user,
		PullPolicy:      launch.pullPolicy,
		Profile:         launch.profile,
		Fresh:           launch.fresh,
		Copy:            launch.copy,
		Privileged:      launch.privileged,
		NoVolumes:       !launch.shareVolumes,
		ReadOnlyVolumes: launch.readOnlyVolumes,
	}
}

func prepareTerminalForDebugSession() {
	// Bubble Tea leaves the alternate screen before returning, which reveals the
	// previous interactive debug shell. Clear and reset the main screen before
	// printing Docker/Kubernetes creation messages for the next selected target.
	_, _ = os.Stdout.WriteString("\033[?25h\033[0m\033[2J\033[H")
}

func newTUIModel(result *tuiLaunch, kubeconfig, kubeContext, namespace string) tuiModel {
	l := list.New(nil, tuiItemDelegate{}, 0, 0)
	l.Title = "targets"
	l.SetShowTitle(false)
	// The custom footer is the single source of truth for key help; the
	// list's built-in help would advertise conflicting bindings.
	l.SetShowHelp(false)
	l.SetStatusBarItemName("entry", "entries")
	// Free the letter aliases (h/l/f/d/b/u) that collide with debux's own
	// keys (toggles, back); arrows and pgup/pgdown keep paginating.
	l.KeyMap.PrevPage = key.NewBinding(key.WithKeys("left", "pgup"))
	l.KeyMap.NextPage = key.NewBinding(key.WithKeys("right", "pgdown"))
	l.Styles.StatusBar = l.Styles.StatusBar.Foreground(tuiMuted)
	l.Styles.FilterPrompt = l.Styles.FilterPrompt.Foreground(tuiAccent2)
	l.Styles.FilterCursor = l.Styles.FilterCursor.Foreground(tuiAccent2)
	l.Styles.PaginationStyle = l.Styles.PaginationStyle.Foreground(tuiMuted)
	l.Styles.HelpStyle = l.Styles.HelpStyle.Foreground(tuiMuted)

	podSearch := textinput.New()
	podSearch.Placeholder = "substring, e.g. webapp-internal-api"
	podSearch.Prompt = "pod search › "
	podSearch.PromptStyle = lipgloss.NewStyle().Foreground(tuiAccent2).Bold(true)
	podSearch.Cursor.Style = lipgloss.NewStyle().Foreground(tuiAccent2)
	podSearch.CharLimit = 128

	return tuiModel{
		list:              l,
		view:              tuiViewDashboard,
		lastAppliedView:   tuiViewDashboard,
		loadGen:           1,
		result:            result,
		loading:           true,
		loadingLabel:      "Loading Docker, kube contexts, and history…",
		selectedContext:   kubeContext,
		selectedNamespace: namespace,
		podSearch:         podSearch,
		image:             result.image,
		user:              result.user,
		pullPolicy:        result.pullPolicy,
		profile:           result.profile,
		fresh:             result.fresh,
		copy:              result.copy,
		privileged:        result.privileged,
		shareVolumes:      result.shareVolumes,
		readOnlyVolumes:   result.readOnlyVolumes,
		kubeconfig:        kubeconfig,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return loadTUIDashboard(m.kubeconfig, m.selectedContext, m.loadGen)
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Handle layout and async load results before any input-mode routing: pod
	// search must not swallow them (stuck "Loading…" and stale sizes), and
	// results from abandoned requests must not overwrite newer state.
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(max(30, msg.Width-4), max(8, msg.Height-14))
		return m, nil
	case tuiDashboardLoadedMsg:
		if msg.gen != m.loadGen {
			return m, nil
		}
		m.loading = false
		m.dockerItems = msg.docker
		m.contextItems = msg.contexts
		m.historyItems = msg.history
		m.dockerErr = msg.dockerErr
		m.contextErr = msg.contextErr
		m.historyErr = msg.historyErr
		m.lastLoadedAt = msg.loadedAt
		return m, m.applyView()
	case tuiNamespacesLoadedMsg:
		if msg.gen != m.loadGen {
			return m, nil
		}
		m.loading = false
		m.namespaceItems = msg.namespaces
		m.namespaceErr = msg.err
		m.selectedContext = msg.context
		return m, m.applyView()
	case tuiPodsLoadedMsg:
		if msg.gen != m.loadGen {
			return m, nil
		}
		m.loading = false
		m.podItems = msg.pods
		m.podsErr = msg.err
		m.podsLimited = msg.limited
		m.selectedContext = msg.context
		m.selectedNamespace = msg.namespace
		m.podQuery = msg.query
		return m, m.applyView()
	case tuiSessionsLoadedMsg:
		if msg.gen != m.loadGen {
			return m, nil
		}
		m.loading = false
		m.sessionItems = msg.sessions
		m.sessionErr = msg.err
		m.sessionsLoaded = true
		m.lastLoadedAt = msg.loadedAt
		return m, m.applyView()
	}

	if m.searchingPods {
		switch msg := msg.(type) {
		case tea.KeyMsg:
			switch msg.String() {
			case "ctrl+c":
				return m, tea.Quit
			case "esc":
				m.searchingPods = false
				return m, nil
			case "enter":
				m.podQuery = strings.TrimSpace(m.podSearch.Value())
				m.searchingPods = false
				m.loading = true
				if m.podQuery == "" {
					m.loadingLabel = "Loading running pods…"
				} else {
					m.loadingLabel = "Searching pods matching “" + m.podQuery + "”…"
				}
				m.loadGen++
				return m, loadTUIPods(m.kubeconfig, m.selectedContext, m.selectedNamespace, m.podQuery, m.loadGen)
			}
		}
		var cmd tea.Cmd
		m.podSearch, cmd = m.podSearch.Update(msg)
		return m, cmd
	}

	if msg, ok := msg.(tea.KeyMsg); ok {
		if !m.list.SettingFilter() {
			switch msg.String() {
			case "ctrl+c", "q":
				return m, tea.Quit
			case "esc":
				// First esc clears an applied filter, the next one goes back —
				// matching the list's own advertised behavior.
				if m.list.FilterState() == list.FilterApplied {
					m.list.ResetFilter()
					return m, nil
				}
				return m, m.goBack()
			case "backspace", "b":
				return m, m.goBack()
			case "tab":
				return m, m.cycleSource(1)
			case "shift+tab":
				return m, m.cycleSource(-1)
			case "home", "0":
				m.view = tuiViewDashboard
				return m, m.applyView()
			case "enter":
				cmd, quit := m.activateSelected(tuiLaunchCurrent)
				if quit {
					return m, tea.Quit
				}
				return m, cmd
			case "t":
				cmd, quit := m.activateSelected(tuiLaunchTerminal)
				if quit {
					return m, tea.Quit
				}
				return m, cmd
			case "1":
				return m, m.openRootView(tuiViewDocker)
			case "2":
				return m, m.openRootView(tuiViewKubeContexts)
			case "3":
				return m, m.openRootView(tuiViewSessions)
			case "4":
				return m, m.openRootView(tuiViewHistory)
			case "r":
				return m, m.reload()
			case "s":
				if m.view == tuiViewKubePods {
					m.searchingPods = true
					m.podSearch.SetValue(m.podQuery)
					m.podSearch.Focus()
					return m, textinput.Blink
				}
			case "f":
				m.fresh = !m.fresh
				return m, nil
			case "c":
				m.copy = !m.copy
				return m, nil
			case "v":
				m.shareVolumes = !m.shareVolumes
				return m, nil
			case "o":
				m.readOnlyVolumes = !m.readOnlyVolumes
				return m, nil
			case "p":
				m.profile = nextProfile(m.profile)
				return m, nil
			case "i":
				m.pullPolicy = nextPullPolicy(m.pullPolicy)
				return m, nil
			}
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m tuiModel) View() string {
	if m.width == 0 {
		return ""
	}
	contentWidth := max(42, m.width-4)
	var b strings.Builder
	b.WriteString(m.headerView(contentWidth))
	b.WriteString("\n")
	b.WriteString(m.tabsView())
	b.WriteString("\n")
	b.WriteString(m.breadcrumbView(contentWidth))
	b.WriteString("\n")
	if warnings := m.warningView(contentWidth); warnings != "" {
		b.WriteString(warnings)
		b.WriteString("\n")
	}
	if m.searchingPods {
		b.WriteString(tuiPanelStyle.Width(contentWidth).Render(m.podSearch.View() + "\n" + tuiHintStyle.Render("enter search whole namespace · esc cancel")))
		b.WriteString("\n")
	} else if m.loading {
		b.WriteString(tuiPanelStyle.Width(contentWidth).Render(tuiHintStyle.Render("⏳ " + m.loadingLabel)))
		b.WriteString("\n")
	} else {
		b.WriteString(tuiPanelStyle.Width(contentWidth).Render(m.list.View()))
		b.WriteString("\n")
	}
	b.WriteString(m.optionsView(contentWidth))
	b.WriteString("\n")
	hint := "/ filter · enter open · tab cycle · 1/2/3/4 jump · b back · f/c/v/o/p/i options · r reload · q quit"
	if m.view == tuiViewKubePods {
		hint = "/ filter · enter open · s search namespace · b back · f/c/v/o/p/i options · r reload · q quit"
	}
	b.WriteString(tuiHintStyle.Render(hint))
	return lipgloss.NewStyle().Padding(1, 2).Render(b.String())
}

func (m tuiModel) headerView(width int) string {
	subtitle := "fast container debugging browser"
	if !m.lastLoadedAt.IsZero() {
		subtitle = "loaded " + m.lastLoadedAt.Format("15:04:05")
	}
	left := lipgloss.JoinHorizontal(lipgloss.Center, tuiLogoStyle.Render("debux"), "  ", tuiTitleStyle.Render("Target browser"), "  ", tuiHintStyle.Render(subtitle))
	right := tuiHintStyle.Render(fmt.Sprintf("%d docker · %d contexts · %d sessions · %d history", len(m.dockerItems), len(m.contextItems), len(m.sessionItems), len(m.historyItems)))
	gap := width - lipgloss.Width(left) - lipgloss.Width(right) - 4
	if gap < 2 {
		gap = 2
	}
	return tuiHeaderStyle.Width(width).Render(left + strings.Repeat(" ", gap) + right)
}

func (m tuiModel) tabsView() string {
	tabs := m.sourceTabs()
	parts := make([]string, len(tabs))
	for i, tab := range tabs {
		count := ""
		if tab.count >= 0 {
			count = fmt.Sprintf(" %d", tab.count)
		}
		label := fmt.Sprintf("%s %s%s", tab.key, tab.label, count)
		if tab.view == m.rootView() {
			parts[i] = tuiActiveTab.Render(label)
		} else {
			parts[i] = tuiTabStyle.Render(label)
		}
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...) + "  " + tuiHintStyle.Render("tab cycle · 1/2/3/4 jump")
}

type tuiSourceTab struct {
	key   string
	view  tuiView
	label string
	count int
}

func (m tuiModel) sourceTabs() []tuiSourceTab {
	sessionCount := -1
	if m.sessionsLoaded {
		sessionCount = len(m.sessionItems)
	}
	return []tuiSourceTab{
		{key: "1", view: tuiViewDocker, label: "Docker", count: len(m.dockerItems)},
		{key: "2", view: tuiViewKubeContexts, label: "Kubernetes", count: len(m.contextItems)},
		{key: "3", view: tuiViewSessions, label: "Sessions", count: sessionCount},
		{key: "4", view: tuiViewHistory, label: "History", count: len(m.historyItems)},
	}
}

func (m *tuiModel) cycleSource(delta int) tea.Cmd {
	tabs := m.sourceTabs()
	current := m.rootView()
	idx := -1
	for i, tab := range tabs {
		if tab.view == current {
			idx = i
			break
		}
	}
	if idx == -1 {
		if delta < 0 {
			idx = len(tabs)
		} else {
			idx = -1
		}
	}
	idx = (idx + delta) % len(tabs)
	if idx < 0 {
		idx += len(tabs)
	}
	m.view = tabs[idx].view
	m.notice = ""
	return m.applyView()
}

func (m tuiModel) rootView() tuiView {
	switch m.view {
	case tuiViewKubeNamespaces, tuiViewKubePods:
		return tuiViewKubeContexts
	default:
		return m.view
	}
}

func (m tuiModel) breadcrumbView(width int) string {
	parts := []string{"Choose a source"}
	switch m.view {
	case tuiViewDocker:
		parts = []string{"Docker"}
	case tuiViewHistory:
		parts = []string{"History"}
	case tuiViewKubeContexts:
		parts = []string{"Kubernetes", "contexts"}
	case tuiViewKubeNamespaces:
		parts = []string{"Kubernetes", emptyAs(m.selectedContext, "current context"), "namespaces"}
	case tuiViewKubePods:
		parts = []string{"Kubernetes", emptyAs(m.selectedContext, "current context"), emptyAs(m.selectedNamespace, "default"), "running pods"}
		if m.podQuery != "" {
			parts = append(parts, "search: "+m.podQuery)
		}
	case tuiViewSessions:
		parts = []string{"Active sessions", emptyAs(m.selectedContext, "current kube context"), emptyAs(m.selectedNamespace, "default namespace")}
	}
	crumb := tuiHintStyle.Render("where ") + tuiSuccessStyle.Render(strings.Join(parts, "  ›  "))
	if m.view == tuiViewKubePods && m.podsLimited {
		crumb += "  " + tuiWarnStyle.Render("showing first results only · press s to search whole namespace")
	}
	return lipgloss.NewStyle().Width(width).Render(crumb)
}

func (m tuiModel) warningView(width int) string {
	var warnings []string
	add := func(label string, err error) {
		if err != nil {
			warnings = append(warnings, label+": "+err.Error())
		}
	}
	switch m.view {
	case tuiViewDocker:
		add("Docker", m.dockerErr)
	case tuiViewKubeContexts:
		add("Kube contexts", m.contextErr)
	case tuiViewKubeNamespaces:
		add("Namespaces", m.namespaceErr)
	case tuiViewKubePods:
		add("Pods", m.podsErr)
	case tuiViewSessions:
		add("Sessions", m.sessionErr)
	case tuiViewHistory:
		add("History", m.historyErr)
	default:
		add("Docker", m.dockerErr)
		add("Kube contexts", m.contextErr)
		add("History", m.historyErr)
	}
	if m.notice != "" {
		warnings = append(warnings, m.notice)
	}
	if len(warnings) == 0 {
		return ""
	}
	return tuiWarnStyle.Width(width).Render("⚠ " + strings.Join(warnings, " · "))
}

func (m tuiModel) optionsView(width int) string {
	pull := m.pullPolicy
	if pull == "" {
		pull = "default"
	}
	pills := []string{
		boolPill("fresh", m.fresh),
		boolPill("copy", m.copy),
		boolPill("volumes", m.shareVolumes),
		boolPill("read-only", m.readOnlyVolumes),
		tuiPillStyle.Render("profile " + m.profile),
		tuiPillStyle.Render("pull " + pull),
	}
	return lipgloss.NewStyle().Width(width).Render(strings.Join(pills, " ") + "  " + tuiHintStyle.Render("f/c/v/o/p/i toggle"))
}

func boolPill(label string, on bool) string {
	if on {
		return tuiPillOnStyle.Render(label + " on")
	}
	return tuiPillStyle.Render(label + " off")
}

func (m *tuiModel) applyView() tea.Cmd {
	switch m.view {
	case tuiViewDocker:
		return m.setItems("Docker containers", m.dockerItems)
	case tuiViewKubeContexts:
		items := append([]tuiItem(nil), m.kubernetesShortcutItems()...)
		items = append(items, m.contextItems...)
		return m.setItems("Kubernetes", items)
	case tuiViewKubeNamespaces:
		return m.setItems("Kubernetes namespaces", m.namespaceItems)
	case tuiViewKubePods:
		return m.setItems("Running pods", m.podItems)
	case tuiViewSessions:
		return m.setItems("Active debux sessions", m.sessionItems)
	case tuiViewHistory:
		return m.setItems("Recent sessions", m.historyItems)
	default:
		return m.setItems("Choose a source", m.sourceItems())
	}
}

func (m tuiModel) sourceItems() []tuiItem {
	sessionsTitle := "Active sessions"
	if m.sessionsLoaded {
		sessionsTitle = fmt.Sprintf("Active sessions  %d", len(m.sessionItems))
	}
	items := []tuiItem{
		{kind: tuiItemSource, source: tuiSourceDocker, view: tuiViewDocker, title: fmt.Sprintf("Docker containers  %d", len(m.dockerItems)), desc: "Local running containers and active debux sidecars · press 1", active: m.rootView() == tuiViewDocker},
		{kind: tuiItemSource, source: tuiSourceK8s, view: tuiViewKubeContexts, title: fmt.Sprintf("Kubernetes  %d contexts", len(m.contextItems)), desc: "Contexts → namespaces → pods, loaded lazily for large clusters · press 2", active: m.rootView() == tuiViewKubeContexts},
		{kind: tuiItemSource, source: tuiSourceSessions, view: tuiViewSessions, title: sessionsTitle, desc: "Reattach to detached Docker sidecars and Kubernetes debug sessions · press 3", active: m.rootView() == tuiViewSessions},
		{kind: tuiItemSource, source: tuiSourceHistory, view: tuiViewHistory, title: fmt.Sprintf("Recent sessions  %d", len(m.historyItems)), desc: "Reopen previously launched debug sessions · press 4", active: m.rootView() == tuiViewHistory},
	}
	if shortcut, ok := m.currentNamespaceShortcut(); ok {
		shortcut.title = "Kubernetes current namespace  " + shortcut.title
		shortcut.desc += " · one-click shortcut"
		items = append(items[:2], append([]tuiItem{shortcut}, items[2:]...)...)
	}
	return items
}

func (m tuiModel) kubernetesShortcutItems() []tuiItem {
	if shortcut, ok := m.currentNamespaceShortcut(); ok {
		shortcut.title = "Current namespace  " + shortcut.title
		return []tuiItem{shortcut}
	}
	return nil
}

func (m tuiModel) currentNamespaceShortcut() (tuiItem, bool) {
	for _, item := range m.contextItems {
		if item.active {
			namespace := item.namespace
			if namespace == "" {
				namespace = runtime.KubernetesDefaultNamespace(m.kubeconfig, item.context)
			}
			return tuiItem{
				kind:      tuiItemKubeNamespace,
				source:    tuiSourceK8s,
				title:     fmt.Sprintf("%s / %s", emptyAs(item.context, "current context"), namespace),
				desc:      "Open running pods in the kubeconfig default namespace",
				context:   item.context,
				namespace: namespace,
				active:    true,
			}, true
		}
	}
	return tuiItem{}, false
}

func (m *tuiModel) setItems(title string, items []tuiItem) tea.Cmd {
	m.list.Title = title
	listItems := make([]list.Item, len(items))
	for i, item := range items {
		listItems[i] = item
	}
	cmd := m.list.SetItems(listItems)
	if m.view != m.lastAppliedView {
		// A view switch starts fresh: the previous view's cursor position
		// could exceed the new list (enter would silently no-op), a persisted
		// filter would invisibly hide entries, and a stale notice would keep
		// rendering.
		m.list.ResetSelected()
		m.list.ResetFilter()
		m.notice = ""
		m.lastAppliedView = m.view
	} else if m.list.Index() >= len(listItems) {
		m.list.ResetSelected()
	}
	return cmd
}

func (m *tuiModel) reload() tea.Cmd {
	m.notice = ""
	m.loading = true
	m.loadGen++
	switch m.view {
	case tuiViewKubeNamespaces:
		m.loadingLabel = "Loading namespaces…"
		return loadTUINamespaces(m.kubeconfig, m.selectedContext, m.selectedNamespace, m.loadGen)
	case tuiViewKubePods:
		m.loadingLabel = "Loading running pods…"
		return loadTUIPods(m.kubeconfig, m.selectedContext, m.selectedNamespace, m.podQuery, m.loadGen)
	case tuiViewSessions:
		m.loadingLabel = "Loading active debux sessions…"
		return loadTUISessions(m.kubeconfig, m.selectedContext, m.selectedNamespace, m.loadGen)
	default:
		m.loadingLabel = "Loading Docker, kube contexts, and history…"
		return loadTUIDashboard(m.kubeconfig, m.selectedContext, m.loadGen)
	}
}

func (m *tuiModel) openRootView(view tuiView) tea.Cmd {
	m.view = view
	m.notice = ""
	if view == tuiViewSessions && !m.sessionsLoaded {
		m.loading = true
		m.loadingLabel = "Loading active debux sessions…"
		m.loadGen++
		return loadTUISessions(m.kubeconfig, m.selectedContext, m.selectedNamespace, m.loadGen)
	}
	return m.applyView()
}

func (m *tuiModel) goBack() tea.Cmd {
	m.notice = ""
	switch m.view {
	case tuiViewKubePods:
		m.view = tuiViewKubeNamespaces
		return m.applyView()
	case tuiViewKubeNamespaces:
		m.view = tuiViewKubeContexts
		return m.applyView()
	case tuiViewDocker, tuiViewKubeContexts, tuiViewSessions, tuiViewHistory:
		m.view = tuiViewDashboard
		return m.applyView()
	default:
		return nil
	}
}

func (m *tuiModel) activateSelected(mode tuiLaunchMode) (tea.Cmd, bool) {
	selected, ok := m.list.SelectedItem().(tuiItem)
	if !ok {
		return nil, false
	}
	if mode == tuiLaunchTerminal {
		if selected.kind != tuiItemTarget {
			m.notice = "Select a concrete Docker container, pod, or history entry before opening externally."
			return nil, false
		}
		if configuredTerminal() == "" {
			m.notice = "External launch is disabled by default. Use enter, or set DEBUX_TERMINAL (or `terminal:` in the config file)."
			return nil, false
		}
	}
	switch selected.kind {
	case tuiItemSource:
		return m.openRootView(selected.view), false
	case tuiItemTarget:
		m.result.target = selected.target
		m.result.mode = mode
		m.result.image = m.image
		m.result.user = m.user
		m.result.pullPolicy = m.pullPolicy
		m.result.profile = m.profile
		if selected.source == tuiSourceSessions {
			if selected.launchImage != "" {
				m.result.image = selected.launchImage
			}
			m.result.user = selected.launchUser
			if selected.launchProfile != "" {
				m.result.profile = selected.launchProfile
			} else {
				m.result.profile = runtime.ProfileGeneral
			}
		}
		m.result.fresh = m.fresh
		m.result.copy = m.copy
		m.result.privileged = m.privileged
		m.result.shareVolumes = m.shareVolumes
		m.result.readOnlyVolumes = m.readOnlyVolumes
		return nil, true
	case tuiItemKubeContext:
		m.selectedContext = selected.context
		m.selectedNamespace = ""
		m.sessionsLoaded = false
		m.view = tuiViewKubeNamespaces
		m.loading = true
		m.loadingLabel = "Loading namespaces for " + emptyAs(selected.context, "current context") + "…"
		m.loadGen++
		return loadTUINamespaces(m.kubeconfig, selected.context, "", m.loadGen), false
	case tuiItemKubeNamespace:
		if selected.context != "" || m.selectedContext == "" {
			m.selectedContext = selected.context
		}
		m.selectedNamespace = selected.namespace
		m.sessionsLoaded = false
		m.podQuery = ""
		m.view = tuiViewKubePods
		m.loading = true
		m.loadingLabel = "Loading running pods in " + selected.namespace + "…"
		m.loadGen++
		return loadTUIPods(m.kubeconfig, m.selectedContext, selected.namespace, "", m.loadGen), false
	default:
		return nil, false
	}
}

func loadTUIDashboard(kubeconfig, preferredContext string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		var dockerItems, contextItems, historyItems []tuiItem
		var dockerErr, contextErr, historyErr error

		if containers, err := runtime.DockerList(ctx); err != nil {
			dockerErr = err
		} else {
			sort.SliceStable(containers, func(i, j int) bool {
				return containers[i].HasDebuxSession && !containers[j].HasDebuxSession
			})
			for _, c := range containers {
				dockerItems = append(dockerItems, tuiDockerItem(c))
			}
		}

		if contexts, err := runtime.KubernetesContexts(kubeconfig); err != nil {
			contextErr = err
			if preferredContext != "" {
				contextItems = append(contextItems, tuiKubeContextItem(runtime.KubeContextInfo{Name: preferredContext, Namespace: runtime.KubernetesDefaultNamespace(kubeconfig, preferredContext)}, preferredContext))
			}
		} else {
			for _, c := range contexts {
				contextItems = append(contextItems, tuiKubeContextItem(c, preferredContext))
			}
			if preferredContext != "" && !containsTUIContext(contextItems, preferredContext) {
				contextItems = append([]tuiItem{tuiKubeContextItem(runtime.KubeContextInfo{Name: preferredContext, Namespace: runtime.KubernetesDefaultNamespace(kubeconfig, preferredContext)}, preferredContext)}, contextItems...)
			}
			sort.SliceStable(contextItems, func(i, j int) bool {
				if contextItems[i].active != contextItems[j].active {
					return contextItems[i].active
				}
				return contextItems[i].title < contextItems[j].title
			})
		}

		if entries, err := history.Load(); err != nil {
			historyErr = err
		} else {
			for _, entry := range entries {
				historyItems = append(historyItems, tuiHistoryItem(entry))
			}
		}

		return tuiDashboardLoadedMsg{gen: gen, docker: dockerItems, contexts: contextItems, history: historyItems, dockerErr: dockerErr, contextErr: contextErr, historyErr: historyErr, loadedAt: time.Now()}
	}
}

func loadTUINamespaces(kubeconfig, kubeContext, preferredNamespace string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		defaultNamespace := preferredNamespace
		if defaultNamespace == "" {
			defaultNamespace = runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext)
		}
		namespaces, err := runtime.KubernetesNamespaces(ctx, kubeconfig, kubeContext)
		if err != nil {
			return tuiNamespacesLoadedMsg{
				gen:        gen,
				context:    kubeContext,
				namespaces: []tuiItem{tuiKubeNamespaceItem(kubeContext, runtime.NamespaceInfo{Name: defaultNamespace, Status: "default"}, defaultNamespace)},
				err:        err,
			}
		}
		items := make([]tuiItem, 0, len(namespaces))
		for _, ns := range namespaces {
			items = append(items, tuiKubeNamespaceItem(kubeContext, ns, defaultNamespace))
		}
		return tuiNamespacesLoadedMsg{gen: gen, context: kubeContext, namespaces: items}
	}
}

func loadTUIPods(kubeconfig, kubeContext, namespace, query string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		pods, limited, err := runtime.KubernetesBrowsePods(ctx, kubeconfig, kubeContext, namespace, query, 300)
		if err != nil {
			return tuiPodsLoadedMsg{gen: gen, context: kubeContext, namespace: namespace, query: query, err: err}
		}
		items := make([]tuiItem, 0, len(pods))
		for _, pod := range pods {
			items = append(items, tuiKubePodItem(pod, kubeContext))
		}
		return tuiPodsLoadedMsg{gen: gen, context: kubeContext, namespace: namespace, query: query, pods: items, limited: limited}
	}
}

func loadTUISessions(kubeconfig, kubeContext, namespace string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		var sessions []runtime.DebugSessionInfo
		var problems []error
		if dockerSessions, err := runtime.DockerSessions(ctx); err != nil {
			problems = append(problems, fmt.Errorf("docker: %w", err))
		} else {
			sessions = append(sessions, dockerSessions...)
		}
		for _, scope := range kubernetesSessionScopes(kubeconfig, kubeContext, namespace, true) {
			if kubeSessions, err := runtime.KubernetesSessions(ctx, kubeconfig, scope.context, scope.namespace, false); err != nil {
				problems = append(problems, fmt.Errorf("kubernetes %s: %w", scope.label(), err))
			} else {
				sessions = append(sessions, kubeSessions...)
			}
		}
		sessions = dedupeDebugSessions(sessions)

		items := make([]tuiItem, 0, len(sessions))
		for _, session := range sessions {
			items = append(items, tuiSessionItem(session))
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].target < items[j].target })

		var err error
		if len(problems) > 0 {
			err = fmt.Errorf("%s", strings.Join(errorStrings(problems), " · "))
		}
		return tuiSessionsLoadedMsg{gen: gen, sessions: items, err: err, loadedAt: time.Now()}
	}
}

func tuiDockerItem(c runtime.ContainerInfo) tuiItem {
	desc := fmt.Sprintf("Docker · %s · %s", c.Image, c.Status)
	return tuiItem{kind: tuiItemTarget, source: tuiSourceDocker, title: c.Name, desc: desc, target: "docker://" + c.Name, active: c.HasDebuxSession}
}

func tuiKubeContextItem(c runtime.KubeContextInfo, preferred string) tuiItem {
	descParts := []string{"Kubernetes context"}
	if c.Namespace != "" {
		descParts = append(descParts, "default ns: "+c.Namespace)
	}
	if c.Cluster != "" {
		descParts = append(descParts, "cluster: "+c.Cluster)
	}
	if c.Current {
		descParts = append(descParts, "current")
	}
	active := c.Current || (preferred != "" && c.Name == preferred)
	return tuiItem{kind: tuiItemKubeContext, source: tuiSourceK8s, title: emptyAs(c.Name, "current context"), desc: strings.Join(descParts, " · "), context: c.Name, namespace: c.Namespace, active: active}
}

func tuiKubeNamespaceItem(kubeContext string, ns runtime.NamespaceInfo, preferred string) tuiItem {
	desc := "Kubernetes namespace"
	if ns.Status != "" {
		desc += " · " + ns.Status
	}
	return tuiItem{kind: tuiItemKubeNamespace, source: tuiSourceK8s, title: ns.Name, desc: desc, context: kubeContext, namespace: ns.Name, active: preferred != "" && ns.Name == preferred}
}

func tuiKubePodItem(p runtime.PodInfo, kubeContext string) tuiItem {
	p.Context = kubeContext
	target := formatTargetURI(&runtime.Target{Runtime: "kubernetes", Context: kubeContext, Namespace: p.Namespace, Name: p.Name})
	descParts := []string{"Kubernetes pod"}
	if kubeContext != "" {
		descParts = append(descParts, "ctx: "+kubeContext)
	}
	if p.Namespace != "" {
		descParts = append(descParts, "ns: "+p.Namespace)
	}
	if p.Status != "" {
		descParts = append(descParts, p.Status)
	}
	return tuiItem{kind: tuiItemTarget, source: tuiSourceK8s, title: p.Name, desc: strings.Join(descParts, " · "), target: target, active: p.HasDebuxSession, context: kubeContext, namespace: p.Namespace}
}

func tuiSessionItem(session runtime.DebugSessionInfo) tuiItem {
	descParts := []string{"Active session", shortDebugSessionKind(session.Kind)}
	if session.DebugName != "" {
		descParts = append(descParts, "debug: "+session.DebugName)
	}
	if session.Source != "" {
		descParts = append(descParts, "source: "+session.Source)
	}
	status := debugSessionStatus(session)
	if status != "" {
		descParts = append(descParts, status)
	}
	return tuiItem{
		kind:          tuiItemTarget,
		source:        tuiSourceSessions,
		title:         session.Target,
		desc:          strings.Join(descParts, " · "),
		target:        session.Target,
		context:       session.Context,
		namespace:     session.Namespace,
		launchImage:   session.Image,
		launchUser:    session.User,
		launchProfile: session.Profile,
		active:        true,
	}
}

func tuiHistoryItem(entry history.Entry) tuiItem {
	title := entry.Target
	if title == "" {
		title = formatTargetURI(&runtime.Target{Runtime: entry.Runtime, Context: entry.Context, Namespace: entry.Namespace, Name: entry.Name, Container: entry.Container})
	}
	desc := fmt.Sprintf("History · %s", entry.StartedAt.Format("2006-01-02 15:04"))
	if entry.Profile != "" {
		desc += " · profile: " + entry.Profile
	}
	if len(entry.Command) > 0 {
		desc += " · -- " + strings.Join(entry.Command, " ")
	}
	return tuiItem{kind: tuiItemTarget, source: tuiSourceHistory, title: title, desc: desc, target: title}
}

func containsTUIContext(items []tuiItem, context string) bool {
	for _, item := range items {
		if item.context == context {
			return true
		}
	}
	return false
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type tuiItemDelegate struct{}

func (d tuiItemDelegate) Height() int  { return 2 }
func (d tuiItemDelegate) Spacing() int { return 1 }
func (d tuiItemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd {
	return nil
}

func (d tuiItemDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	i, ok := item.(tuiItem)
	if !ok || m.Width() <= 0 {
		return
	}
	selected := index == m.Index()
	width := max(20, m.Width()-4)
	icon := tuiItemIcon(i)
	titleStyle := lipgloss.NewStyle().Bold(true).Foreground(tuiText)
	descStyle := lipgloss.NewStyle().Foreground(tuiMuted)
	rowStyle := lipgloss.NewStyle().Padding(0, 1)
	if selected && m.FilterState() != list.Filtering {
		titleStyle = titleStyle.Foreground(tuiSelectedText)
		descStyle = descStyle.Foreground(tuiSelectedDesc)
		rowStyle = rowStyle.Border(lipgloss.NormalBorder(), false, false, false, true).BorderForeground(tuiAccent).Background(tuiSelectedBg)
	} else if i.active {
		titleStyle = titleStyle.Foreground(tuiActiveTitle)
	}

	title := icon + " " + i.title
	if i.active {
		title += "  active"
	}
	if i.kind != tuiItemTarget {
		title += "  ›"
	}
	title = ansi.Truncate(title, width, "…")
	desc := ansi.Truncate(i.desc, width, "…")
	_, _ = fmt.Fprint(w, rowStyle.Width(width).Render(titleStyle.Render(title)+"\n"+descStyle.Render(desc)))
}

func tuiItemIcon(i tuiItem) string {
	switch i.kind {
	case tuiItemKubeContext:
		return "☸"
	case tuiItemKubeNamespace:
		return "◇"
	}
	switch i.source {
	case tuiSourceDocker:
		return "🐳"
	case tuiSourceK8s:
		return "☸"
	case tuiSourceSessions:
		return "●"
	case tuiSourceHistory:
		return "↺"
	default:
		return "•"
	}
}

func nextProfile(current string) string {
	if current == "" {
		return runtime.ValidProfiles[0]
	}
	for i, profile := range runtime.ValidProfiles {
		if profile == current {
			return runtime.ValidProfiles[(i+1)%len(runtime.ValidProfiles)]
		}
	}
	return runtime.ValidProfiles[0]
}

func nextPullPolicy(current string) string {
	switch current {
	case "":
		return "Always"
	case "Always":
		return "IfNotPresent"
	case "IfNotPresent":
		return "Never"
	default:
		return ""
	}
}

// configuredTerminal returns the external terminal command: DEBUX_TERMINAL
// wins, then the config file's `terminal:` value.
func configuredTerminal() string {
	if t := strings.TrimSpace(os.Getenv("DEBUX_TERMINAL")); t != "" {
		return t
	}
	return strings.TrimSpace(config.Get().Terminal)
}

func openInTerminal(ctx context.Context, cmd *cobra.Command, launch tuiLaunch) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving debux executable: %w", err)
	}
	commandLine := terminalShellCommand(append([]string{exe}, buildExecArgs(cmd, launch)...))

	terminal := configuredTerminal()
	if terminal == "" {
		return fmt.Errorf("external launch is disabled by default; press enter to open in the current terminal, or set DEBUX_TERMINAL (or `terminal:` in the config file)")
	}
	if err := startConfiguredTerminal(ctx, terminal, commandLine); err != nil {
		return err
	}
	fmt.Printf("Opened %s with DEBUX_TERMINAL\n", launch.target)
	return nil
}

func startConfiguredTerminal(ctx context.Context, terminal, commandLine string) error {
	if strings.Contains(terminal, "{command}") {
		cmdline := strings.ReplaceAll(terminal, "{command}", terminalShellQuote(commandLine))
		proc := exec.CommandContext(ctx, "sh", "-lc", cmdline)
		return proc.Start()
	}
	return startTerminalExecutable(ctx, terminal, commandLine)
}

func startTerminalExecutable(ctx context.Context, terminal, commandLine string) error {
	fields := strings.Fields(terminal)
	if len(fields) == 0 {
		return fmt.Errorf("empty terminal command")
	}
	path, err := exec.LookPath(fields[0])
	if err != nil {
		return err
	}
	args := terminalArgs(filepath.Base(fields[0]), fields[1:], commandLine)
	proc := exec.CommandContext(ctx, path, args...)
	return proc.Start()
}

func terminalArgs(name string, prefix []string, commandLine string) []string {
	args := append([]string(nil), prefix...)
	switch name {
	case "wezterm":
		return append(args, "start", "--", "sh", "-lc", commandLine)
	case "kitty":
		return append(args, "sh", "-lc", commandLine)
	case "gnome-terminal":
		return append(args, "--", "sh", "-lc", commandLine)
	default:
		return append(args, "-e", "sh", "-lc", commandLine)
	}
}

func terminalShellCommand(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = terminalShellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func terminalShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func buildExecArgs(cmd *cobra.Command, launch tuiLaunch) []string {
	args := []string{"exec", launch.target}
	isKubernetes := strings.HasPrefix(launch.target, "k8s://")
	if launch.image != "" && launch.image != runtime.DefaultImage {
		args = append(args, "--image", launch.image)
	}
	if launch.fresh {
		args = append(args, "--fresh")
	}
	if !launch.shareVolumes {
		args = append(args, "--no-volumes")
	}
	if launch.readOnlyVolumes {
		args = append(args, "--read-only-volumes")
	}
	if launch.user != "" {
		args = append(args, "--user", launch.user)
	}
	if launch.privileged {
		args = append(args, "--privileged")
	}
	if launch.pullPolicy != "" {
		args = append(args, "--pull-policy", launch.pullPolicy)
	}
	if isKubernetes {
		if launch.copy {
			args = append(args, "--copy")
		}
		if launch.profile != "" && launch.profile != runtime.ProfileGeneral {
			args = append(args, "--profile", launch.profile)
		}
		if kubeconfig, _ := cmd.Flags().GetString("kubeconfig"); kubeconfig != "" {
			args = append(args, "--kubeconfig", kubeconfig)
		}
		if flagKubeContext != "" && !strings.Contains(launch.target, "k8s://@") {
			args = append(args, "--context", flagKubeContext)
		}
		if flagNamespace != "" && strings.Count(strings.TrimPrefix(launch.target, "k8s://"), "/") == 0 {
			args = append(args, "--namespace", flagNamespace)
		}
	}
	return args
}

func formatTargetURI(target *runtime.Target) string {
	switch target.Runtime {
	case "docker":
		return "docker://" + target.Name
	case "containerd":
		return "containerd://" + target.Name
	case "kubernetes":
		parts := make([]string, 0, 4)
		prefix := "k8s://"
		if target.Context != "" {
			prefix += "@" + url.PathEscape(target.Context)
		}
		if target.Namespace != "" {
			parts = append(parts, url.PathEscape(target.Namespace))
		}
		if target.Name != "" {
			parts = append(parts, url.PathEscape(target.Name))
		}
		if target.Container != "" {
			parts = append(parts, url.PathEscape(target.Container))
		}
		if len(parts) == 0 {
			return prefix
		}
		if target.Context != "" {
			return prefix + "/" + strings.Join(parts, "/")
		}
		return prefix + strings.Join(parts, "/")
	default:
		return target.Name
	}
}
