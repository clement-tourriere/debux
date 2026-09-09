package cli

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func completeKubernetesTarget(cmd *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	kubeconfig := completionFlagString(cmd, "kubeconfig")
	rest := strings.TrimPrefix(toComplete, "k8s://")
	if strings.HasPrefix(rest, "@") {
		completions := completeKubernetesContextTarget(cmd, kubeconfig, rest, toComplete)
		return uniqueCompletions(completions), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveKeepOrder
	}

	kubeContext := completionSelectedKubeContext(cmd)
	completions := completeKubernetesPath(cmd, kubeconfig, kubeContext, "k8s://", rest, toComplete, true)
	return uniqueCompletions(completions), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace | cobra.ShellCompDirectiveKeepOrder
}

func completeKubernetesContextTarget(cmd *cobra.Command, kubeconfig, rest, toComplete string) []string {
	withoutMarker := strings.TrimPrefix(rest, "@")
	contextPart, path, hasPath := strings.Cut(withoutMarker, "/")
	if !hasPath {
		return completeKubernetesContexts(kubeconfig, "k8s://@", contextPart, toComplete)
	}

	kubeContext, err := url.PathUnescape(contextPart)
	if err != nil || kubeContext == "" {
		return appendActiveHelp(nil, "Invalid Kubernetes context in target")
	}
	base := "k8s://@" + url.PathEscape(kubeContext) + "/"
	return completeKubernetesPath(cmd, kubeconfig, kubeContext, base, path, toComplete, true)
}

func completeKubernetesPath(cmd *cobra.Command, kubeconfig, kubeContext, base, path, toComplete string, includeContexts bool) []string {
	parts := strings.Split(path, "/")
	explicitContext := strings.HasPrefix(base, "k8s://@")

	// Make the high-level URI hierarchy predictable:
	//   k8s://<TAB>            -> contexts only
	//   k8s://@ctx/<TAB>       -> namespaces only
	//   k8s://@ctx/ns/<TAB>    -> pods only
	//   k8s://@ctx/ns/pod/<TAB> -> containers only
	if includeContexts && base == "k8s://" && path == "" {
		return completeKubernetesContexts(kubeconfig, "k8s://@", "", toComplete)
	}
	if explicitContext && len(parts) == 1 {
		return completeKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete)
	}

	switch len(parts) {
	case 1:
		typedPart := unescapeCompletionPart(parts[0])
		var completions []string
		if !completionNamespaceFlagChanged(cmd) && typedPart == "" {
			completions = append(completions, completeKubernetesDefaultNamespace(kubeconfig, kubeContext, base, toComplete)...)
		}

		namespace := completionSelectedNamespace(cmd, kubeconfig, kubeContext)
		completions = append(completions, completeKubernetesPods(kubeconfig, kubeContext, namespace, base, typedPart, toComplete)...)

		// Namespace listing can be slow on large clusters. Once the user has typed
		// a reasonably specific token, prefer fast pod substring completion. Typing
		// one or two namespace characters (for example `k8s://gi<TAB>`) still opens
		// namespace discovery.
		if !completionNamespaceFlagChanged(cmd) && typedPart != "" && len(typedPart) < completionSubstringMinLength {
			completions = append(completions, completeKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete)...)
		}
		return completions
	case 2:
		first := unescapeCompletionPart(parts[0])
		if first == "" {
			return nil
		}
		second := unescapeCompletionPart(parts[1])
		if explicitContext || parts[1] == "" {
			podBase := base + url.PathEscape(first) + "/"
			return completeKubernetesPods(kubeconfig, kubeContext, first, podBase, second, toComplete)
		}
		selectedNamespace := completionSelectedNamespace(cmd, kubeconfig, kubeContext)
		if completionNamespaceFlagChanged(cmd) && first != selectedNamespace {
			podBase := base + url.PathEscape(first)
			return completeKubernetesContainers(kubeconfig, kubeContext, selectedNamespace, first, podBase, second, toComplete)
		}
		podBase := base + url.PathEscape(first) + "/"
		return completeKubernetesPods(kubeconfig, kubeContext, first, podBase, second, toComplete)
	case 3:
		namespace := unescapeCompletionPart(parts[0])
		podName := unescapeCompletionPart(parts[1])
		containerPrefix := unescapeCompletionPart(parts[2])
		if namespace == "" || podName == "" {
			return nil
		}
		podBase := base + url.PathEscape(namespace) + "/" + url.PathEscape(podName)
		return completeKubernetesContainers(kubeconfig, kubeContext, namespace, podName, podBase, containerPrefix, toComplete)
	default:
		return nil
	}
}

func completeKubernetesContexts(kubeconfig, base, typedContext, toComplete string) []string {
	contexts, err := runtime.KubernetesContexts(kubeconfig)
	if err != nil {
		return appendActiveHelp(nil, "Kubernetes context completion unavailable: "+err.Error())
	}
	var completions []string
	for _, c := range contexts {
		if typedContext != "" && !strings.HasPrefix(c.Name, typedContext) && !strings.HasPrefix(url.PathEscape(c.Name), typedContext) {
			continue
		}
		desc := "Kubernetes context"
		if c.Namespace != "" {
			desc += " · default ns: " + c.Namespace
		}
		if c.Cluster != "" {
			desc += " · cluster: " + c.Cluster
		}
		if c.Current {
			desc += " · current"
		}
		value := c.Name
		if base != "" {
			value = base + url.PathEscape(c.Name) + "/"
		}
		completions = appendCompletion(completions, value, desc, toComplete)
	}
	if len(completions) == 0 && typedContext == "" {
		completions = appendActiveHelp(completions, "No kubeconfig contexts found")
	}
	return completions
}

func completeKubernetesDefaultNamespace(kubeconfig, kubeContext, base, toComplete string) []string {
	namespace := runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext)
	if namespace == "" {
		return nil
	}
	desc := "Default Kubernetes namespace"
	if kubeContext != "" {
		desc += " · ctx: " + kubeContext
	}
	return appendCompletion(nil, base+url.PathEscape(namespace)+"/", desc, toComplete)
}

func completeKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete string) []string {
	if cache, ok := readCompletionNamespaceCache(kubeconfig, kubeContext); ok {
		completions := formatKubernetesNamespaceCompletions(cache.Namespaces, base, toComplete)
		if time.Since(cache.SavedAt) > completionNamespaceCacheFreshFor {
			if startCompletionNamespaceCacheRefresh(kubeconfig, kubeContext) {
				completions = appendActiveHelp(completions, "Using cached namespaces; refreshing in the background")
			} else {
				completions = appendActiveHelp(completions, "Using cached namespaces")
			}
		}
		if cache.Limited {
			completions = appendActiveHelp(completions, "Cached namespace list was limited; keep typing to narrow the search")
		}
		return completions
	}

	namespaces, timedOut, err := listKubernetesNamespacesForCompletion(kubeconfig, kubeContext)
	if timedOut {
		completions := completeKnownKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete)
		if startCompletionNamespaceCacheRefresh(kubeconfig, kubeContext) {
			return appendActiveHelp(completions, "Namespace lookup is slow; showing default/recent namespaces and refreshing in the background — press Tab again in a few seconds")
		}
		return appendActiveHelp(completions, "Namespace lookup is slow; showing default/recent namespaces")
	}
	if err != nil {
		completions := completeKnownKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete)
		if startCompletionNamespaceCacheRefresh(kubeconfig, kubeContext) {
			completions = appendActiveHelp(completions, "Refreshing namespace cache in the background")
		}
		return appendActiveHelp(completions, "Kubernetes namespace completion unavailable: "+err.Error())
	}
	_ = writeCompletionNamespaceCache(kubeconfig, kubeContext, namespaces, false)
	return formatKubernetesNamespaceCompletions(namespaces, base, toComplete)
}

func formatKubernetesNamespaceCompletions(namespaces []runtime.NamespaceInfo, base, toComplete string) []string {
	var completions []string
	for _, ns := range namespaces {
		desc := "Kubernetes namespace"
		if ns.Status != "" {
			desc += " · " + ns.Status
		}
		completions = appendCompletion(completions, base+url.PathEscape(ns.Name)+"/", desc, toComplete)
	}
	return completions
}

func completeKnownKubernetesNamespaces(kubeconfig, kubeContext, base, toComplete string) []string {
	var completions []string
	seen := make(map[string]struct{})
	add := func(namespace, desc string) {
		namespace = strings.TrimSpace(namespace)
		if namespace == "" {
			return
		}
		if _, ok := seen[namespace]; ok {
			return
		}
		seen[namespace] = struct{}{}
		completions = appendCompletion(completions, base+url.PathEscape(namespace)+"/", desc, toComplete)
	}

	add(runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext), "Default Kubernetes namespace")
	resolvedContext := completionCacheKubeContext(kubeconfig, kubeContext)
	entries, err := history.Load()
	if err != nil {
		return completions
	}
	for _, entry := range entries {
		if entry.Runtime != "kubernetes" || entry.Namespace == "" {
			continue
		}
		if resolvedContext != "" && entry.Context != "" && entry.Context != resolvedContext {
			continue
		}
		if kubeContext != "" && entry.Context == "" {
			continue
		}
		add(entry.Namespace, "Recent Kubernetes namespace")
	}
	return completions
}

func completeKubernetesPods(kubeconfig, kubeContext, namespace, base, query, toComplete string) []string {
	if namespace == "" {
		namespace = runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext)
	}
	displayContext := completionDisplayKubeContext(kubeconfig, kubeContext)

	// A limited (truncated) cache cannot answer substring queries
	// authoritatively: matches beyond the cached page would stay hidden for
	// the cache's lifetime. Use it only for blank-query browsing and fall
	// through to a live lookup otherwise.
	if cache, ok := readCompletionPodCache(kubeconfig, kubeContext, namespace); ok && (!cache.Limited || strings.TrimSpace(query) == "") {
		pods := filterAndSortPodsForCompletion(cache.Pods, namespace, query)
		if len(pods) > 0 || strings.TrimSpace(query) == "" {
			completions := appendKubernetesPodScopeHelp(nil, displayContext, namespace)
			completions = append(completions, formatKubernetesPodCompletions(pods, base, query, toComplete, displayContext, namespace)...)
			if time.Since(cache.SavedAt) > completionPodCacheFreshFor {
				if startCompletionPodCacheRefresh(kubeconfig, kubeContext, namespace) {
					completions = appendActiveHelp(completions, fmt.Sprintf("Using cached pods from %s/%s; refreshing in the background", displayContext, namespace))
				} else {
					completions = appendActiveHelp(completions, fmt.Sprintf("Using cached pods from %s/%s", displayContext, namespace))
				}
			} else {
				completions = appendKubernetesPodScopeHelp(completions, displayContext, namespace)
			}
			if cache.Limited {
				completions = appendActiveHelp(completions, "Cached pod list was limited; keep typing to narrow the search")
			}
			return completions
		}
	}

	maxResults := completionMaxResults
	if strings.TrimSpace(query) == "" {
		maxResults = completionBlankPodMaxResults
	}
	pods, limited, timedOut, err := browseKubernetesPodsForCompletion(kubeconfig, kubeContext, namespace, query, maxResults)
	if timedOut {
		completions := appendKubernetesPodScopeHelp(nil, displayContext, namespace)
		if startCompletionPodCacheRefresh(kubeconfig, kubeContext, namespace) {
			return appendActiveHelp(completions, "Pod lookup is slow; warming a local cache in the background — press Tab again in a few seconds")
		}
		return appendActiveHelp(completions, "Pod lookup is slow; use a more specific pod substring or try again")
	}
	if err != nil {
		completions := appendKubernetesPodScopeHelp(nil, displayContext, namespace)
		if startCompletionPodCacheRefresh(kubeconfig, kubeContext, namespace) {
			completions = appendActiveHelp(completions, "Refreshing pod cache in the background")
		}
		return appendActiveHelp(completions, "Kubernetes pod completion unavailable: "+err.Error())
	}
	pods = filterAndSortPodsForCompletion(pods, namespace, query)
	if strings.TrimSpace(query) == "" {
		_ = writeCompletionPodCache(kubeconfig, kubeContext, namespace, pods, limited)
	} else {
		_ = startCompletionPodCacheRefresh(kubeconfig, kubeContext, namespace)
	}
	completions := appendKubernetesPodScopeHelp(nil, displayContext, namespace)
	completions = append(completions, formatKubernetesPodCompletions(pods, base, query, toComplete, displayContext, namespace)...)
	if limited {
		completions = appendActiveHelp(completions, fmt.Sprintf("Showing first %d pods in %s/%s; keep typing to narrow the search", maxResults, displayContext, namespace))
	}
	return completions
}

func completeKubernetesContainers(kubeconfig, kubeContext, namespace, podName, podBase, containerPrefix, toComplete string) []string {
	containers, timedOut, err := listKubernetesContainersForCompletion(kubeconfig, kubeContext, namespace, podName)
	if timedOut {
		return appendActiveHelp(nil, "Container completion is taking too long; run the pod target without /<container> to pick interactively")
	}
	if err != nil {
		return appendActiveHelp(nil, "Kubernetes container completion unavailable: "+err.Error())
	}
	var completions []string
	for _, name := range containers {
		if containerPrefix != "" && !strings.HasPrefix(name, containerPrefix) && !strings.HasPrefix(url.PathEscape(name), containerPrefix) {
			continue
		}
		completions = appendCompletion(completions, podBase+"/"+url.PathEscape(name), "Kubernetes container", toComplete)
	}
	return completions
}

func completeKubeContextFlag(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	completions := completeKubernetesContexts(completionFlagString(cmd, "kubeconfig"), "", toComplete, toComplete)
	return uniqueSortedCompletions(completions), cobra.ShellCompDirectiveNoFileComp
}

func completeKubeNamespaceFlag(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	kubeconfig := completionFlagString(cmd, "kubeconfig")
	kubeContext := completionSelectedKubeContext(cmd)
	namespaces, timedOut, err := listKubernetesNamespacesForCompletion(kubeconfig, kubeContext)
	if timedOut {
		return appendActiveHelp(nil, "Namespace completion is taking too long"), cobra.ShellCompDirectiveNoFileComp
	}
	if err != nil {
		return appendActiveHelp(nil, "Kubernetes namespace completion unavailable: "+err.Error()), cobra.ShellCompDirectiveNoFileComp
	}
	var completions []string
	for _, ns := range namespaces {
		desc := "Kubernetes namespace"
		if ns.Status != "" {
			desc += " · " + ns.Status
		}
		completions = appendCompletion(completions, ns.Name, desc, toComplete)
	}
	return uniqueSortedCompletions(completions), cobra.ShellCompDirectiveNoFileComp
}
