package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
	session         *runtime.DebugSessionInfo
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
	session       *runtime.DebugSessionInfo
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
		m.result.session = selected.session
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
