package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

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
	return m.openRootView(tabs[idx].view)
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
