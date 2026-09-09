package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/runtime"
)

func browseKubernetesPodsForCompletion(kubeconfig, kubeContext, namespace, query string, maxResults int) ([]runtime.PodInfo, bool, bool, error) {
	type result struct {
		pods    []runtime.PodInfo
		limited bool
		err     error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan result, 1)
	go func() {
		pods, limited, err := runtime.KubernetesBrowsePods(ctx, kubeconfig, kubeContext, namespace, query, maxResults)
		ch <- result{pods: pods, limited: limited, err: err}
	}()

	select {
	case res := <-ch:
		return res.pods, res.limited, false, res.err
	case <-time.After(completionTimeout):
		cancel()
		return nil, false, true, nil
	}
}

func listKubernetesNamespacesForCompletion(kubeconfig, kubeContext string) ([]runtime.NamespaceInfo, bool, error) {
	type result struct {
		namespaces []runtime.NamespaceInfo
		limited    bool
		err        error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan result, 1)
	go func() {
		namespaces, limited, err := runtime.KubernetesBrowseNamespaces(ctx, kubeconfig, kubeContext, "", completionNamespaceCacheMaxResults)
		if err == nil {
			_ = writeCompletionNamespaceCache(kubeconfig, kubeContext, namespaces, limited)
		}
		ch <- result{namespaces: namespaces, limited: limited, err: err}
	}()

	select {
	case res := <-ch:
		return res.namespaces, false, res.err
	case <-time.After(completionTimeout):
		cancel()
		return nil, true, nil
	}
}

func listKubernetesContainersForCompletion(kubeconfig, kubeContext, namespace, podName string) ([]string, bool, error) {
	type result struct {
		containers []string
		err        error
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan result, 1)
	go func() {
		containers, err := runtime.KubernetesRunningContainers(ctx, kubeconfig, kubeContext, namespace, podName)
		ch <- result{containers: containers, err: err}
	}()

	select {
	case res := <-ch:
		return res.containers, false, res.err
	case <-time.After(completionTimeout):
		cancel()
		return nil, true, nil
	}
}

func filterAndSortPodsForCompletion(pods []runtime.PodInfo, namespace, query string) []runtime.PodInfo {
	query = strings.ToLower(strings.TrimSpace(query))
	filtered := make([]runtime.PodInfo, 0, len(pods))
	for _, pod := range pods {
		if namespace != "" && pod.Namespace != "" && pod.Namespace != namespace {
			continue
		}
		if podCompletionMatches(pod.Namespace, pod.Name, query) {
			filtered = append(filtered, pod)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		ri := podCompletionRank(filtered[i].Namespace, filtered[i].Name, query)
		rj := podCompletionRank(filtered[j].Namespace, filtered[j].Name, query)
		if ri != rj {
			return ri < rj
		}
		return filtered[i].Name < filtered[j].Name
	})
	return filtered
}

func podCompletionMatches(namespace, name, query string) bool {
	if query == "" {
		return true
	}
	name = strings.ToLower(name)
	namespaced := strings.ToLower(namespace + "/" + name)
	if strings.HasPrefix(name, query) || strings.HasPrefix(namespaced, query) {
		return true
	}
	return len(query) >= completionSubstringMinLength && (strings.Contains(name, query) || strings.Contains(namespaced, query))
}

func podCompletionRank(namespace, name, query string) int {
	if query == "" {
		return 0
	}
	name = strings.ToLower(name)
	namespaced := strings.ToLower(namespace + "/" + name)
	switch {
	case strings.HasPrefix(name, query):
		return 0
	case strings.HasPrefix(namespaced, query):
		return 1
	case strings.Contains(name, "-"+query) || strings.Contains(name, "/"+query):
		return 2
	case strings.Contains(name, query) || strings.Contains(namespaced, query):
		return 3
	default:
		return 4
	}
}

func formatKubernetesPodCompletions(pods []runtime.PodInfo, base, query, toComplete, displayContext, namespace string) []string {
	var completions []string
	for _, pod := range pods {
		podNamespace := pod.Namespace
		if podNamespace == "" {
			podNamespace = namespace
		}
		desc := kubernetesPodCompletionDescription(pod, displayContext, podNamespace)
		value := base + url.PathEscape(pod.Name)
		completions = appendPodCompletion(completions, value, desc, query, toComplete)
	}
	return completions
}

func kubernetesPodCompletionDescription(pod runtime.PodInfo, displayContext, namespace string) string {
	parts := []string{"pod", "ns:" + namespace}
	if displayContext != "" {
		parts = append(parts, "ctx:"+displayContext)
	}
	if workload := kubernetesPodWorkloadName(pod.Name); workload != "" && workload != pod.Name {
		parts = append(parts, workload)
	}
	if pod.HasDebuxSession {
		parts = append(parts, "active debux")
	}
	return "☸ " + strings.Join(parts, " · ")
}

func kubernetesPodWorkloadName(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) >= 3 && isKubernetesPodSuffix(parts[len(parts)-1]) && isKubernetesReplicaSetHash(parts[len(parts)-2]) {
		return strings.Join(parts[:len(parts)-2], "-")
	}
	if len(parts) >= 2 && isKubernetesOrdinal(parts[len(parts)-1]) {
		return strings.Join(parts[:len(parts)-1], "-")
	}
	return name
}

func isKubernetesPodSuffix(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func isKubernetesReplicaSetHash(s string) bool {
	if len(s) < 8 || len(s) > 10 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func isKubernetesOrdinal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func appendPodCompletion(completions []string, value, desc, query, toComplete string) []string {
	if strings.HasPrefix(value, toComplete) || len(strings.TrimSpace(query)) >= completionSubstringMinLength {
		if desc == "" {
			return append(completions, value)
		}
		return append(completions, value+"\t"+desc)
	}
	return completions
}

func appendKubernetesPodScopeHelp(completions []string, displayContext, namespace string) []string {
	return appendActiveHelp(completions, fmt.Sprintf("☸ pods from ctx:%s ns:%s — type <namespace>/ to switch namespace; 3+ chars also match substrings", displayContext, namespace))
}

func completionDisplayKubeContext(kubeconfig, kubeContext string) string {
	if kubeContext != "" {
		return kubeContext
	}
	current, err := runtime.KubernetesCurrentContext(kubeconfig)
	if err != nil || current == "" {
		return "current"
	}
	return current
}
