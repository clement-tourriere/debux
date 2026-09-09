package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/runtime"
)

func loadTUIDashboard(kubeconfig, preferredContext string, gen int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()

		var dockerItems, contextItems, historyItems []tuiItem
		var dockerErr, contextErr, historyErr error

		if containers, err := runtime.DockerList(ctx, nil); err != nil {
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
		if dockerSessions, err := runtime.DockerSessions(ctx, nil); err != nil {
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
		session:       &session,
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
