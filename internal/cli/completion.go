package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

const (
	completionTimeout            = 650 * time.Millisecond
	completionMaxResults         = 200
	completionBlankPodMaxResults = 50
	completionSubstringMinLength = 3
)

func newCompletionCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for debux.

Load the generated script from your shell profile, or write it to your shell's
completion directory. The generated completions include live Docker container
and Kubernetes context/namespace/pod/container suggestions for debux targets.`,
		Example: `  debux completion zsh > ~/.zfunc/_debux
  debux completion bash > /etc/bash_completion.d/debux
  debux completion fish > ~/.config/fish/completions/debux.fish`,
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			return completeFixedValues(toComplete, []completionChoice{
				{value: "bash", desc: "Bash completion"},
				{value: "zsh", desc: "Zsh completion"},
				{value: "fish", desc: "fish completion"},
				{value: "powershell", desc: "PowerShell completion"},
			})
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return root.GenBashCompletion(cmd.OutOrStdout())
			case "zsh":
				return genZshCompletionWithSubstringMatching(root, cmd)
			case "fish":
				return root.GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return root.GenPowerShellCompletion(cmd.OutOrStdout())
			default:
				return fmt.Errorf("unsupported shell %q: expected bash, zsh, fish, or powershell", args[0])
			}
		},
	}
	return cmd
}

func genZshCompletionWithSubstringMatching(root *cobra.Command, cmd *cobra.Command) error {
	var buf bytes.Buffer
	if err := root.GenZshCompletion(&buf); err != nil {
		return err
	}
	_, err := cmd.OutOrStdout().Write([]byte(patchZshCompletionForSubstringMatches(buf.String())))
	return err
}

func patchZshCompletionForSubstringMatches(script string) string {
	patched, ok := tryPatchZshCompletionForSubstringMatches(script)
	if !ok {
		// A cobra upgrade moved the anchors this patch targets. Serving the
		// stock script keeps every completion working with normal prefix
		// matching; a half-applied patch would break ALL dynamic zsh
		// completions silently.
		return script
	}
	return patched
}

func tryPatchZshCompletionForSubstringMatches(script string) (string, bool) {
	// Cobra's stock zsh script feeds candidates to _describe. _describe asks zsh
	// to apply normal prefix matching, so it hides intentional pod substring
	// matches such as `k8s://inter` -> `k8s://webapp-internal-api-...`.
	// Capture raw values/descriptions and use compadd -U instead. The Go
	// completion code remains responsible for filtering candidates.
	var ok bool
	script, ok = replaceExactlyOnce(script,
		"local -a completions",
		"local -a completions completionValues completionDescriptions",
	)
	if !ok {
		return "", false
	}
	script, ok = replaceExactlyOnce(script,
		`            # If requested, completions are returned with a description.
            # The description is preceded by a TAB character.
            # For zsh's _describe, we need to use a : instead of a TAB.
            # We first need to escape any : as part of the completion itself.
            comp=${comp//:/\\:}

            local tab="$(printf '\t')"
            comp=${comp//$tab/:}

            __debux_debug "Adding completion: ${comp}"
            completions+=${comp}
            lastComp=$comp`,
		`            # If requested, completions are returned with a description.
            # The description is preceded by a TAB character.
            local tab="$(printf '\t')"
            local value="${comp%%$tab*}"
            local desc=""
            if [[ "$comp" == *$tab* ]]; then
                desc="${comp#*$tab}"
            fi
            completionValues+=("${value}")
            completionDescriptions+=("${desc}")

            # Keep Cobra's original _describe-compatible array around for
            # fallback paths and debug output.
            comp=${comp//:/\\:}
            comp=${comp//$tab/:}

            __debux_debug "Adding completion: ${comp}"
            completions+=${comp}
            lastComp=$comp`,
	)
	if !ok {
		return "", false
	}
	script, ok = replaceExactlyOnce(script,
		"        __debux_debug \"Calling _describe\"\n        if eval _describe $keepOrder \"completions\" completions $flagPrefix $noSpace; then",
		`        __debux_debug "Calling compadd for dynamic completions"
        local -a completionDisplayValues
        local completionIndex
        for (( completionIndex=1; completionIndex<=${#completionValues}; completionIndex++ )); do
            if [ -n "${completionDescriptions[$completionIndex]}" ]; then
                completionDisplayValues+=("${completionValues[$completionIndex]}  -- ${completionDescriptions[$completionIndex]}")
            else
                completionDisplayValues+=("${completionValues[$completionIndex]}")
            fi
        done
        if [ ${#completionValues} -ne 0 ] && eval compadd -U -V completions -d completionDisplayValues $flagPrefix $noSpace -a completionValues; then`,
	)
	if !ok {
		return "", false
	}
	script = strings.ReplaceAll(script, `__debux_debug "_describe found some completions"`, `__debux_debug "compadd found some completions"`)
	script = strings.ReplaceAll(script, `__debux_debug "_describe did not find completions."`, `__debux_debug "compadd did not find completions."`)
	return script, true
}

// replaceExactlyOnce replaces old with new and reports whether the anchor was
// actually found, so template drift in a dependency fails loudly instead of
// producing a half-patched script.
func replaceExactlyOnce(s, old, new string) (string, bool) {
	if !strings.Contains(s, old) {
		return s, false
	}
	return strings.Replace(s, old, new, 1), true
}

func configureTargetCompletion(cmd *cobra.Command) {
	cmd.ValidArgsFunction = completeTargetArg
}

func configureImageArgCompletion(cmd *cobra.Command) {
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveDefault
		}
		return completeDockerImageRefs(toComplete, false)
	}
}

func registerExecFlagCompletions(cmd *cobra.Command) {
	registerImageFlagCompletion(cmd)
	registerKubernetesFlagCompletions(cmd)
	registerPullPolicyFlagCompletion(cmd)
	registerProfileFlagCompletion(cmd)
}

func registerImageFlagCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("image", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeDockerImages(cmd, toComplete)
	})
}

func registerKubernetesFlagCompletions(cmd *cobra.Command) {
	registerKubeContextFlagCompletion(cmd)
	registerNamespaceFlagCompletion(cmd)
}

func registerKubeContextFlagCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("context", completeKubeContextFlag)
}

func registerNamespaceFlagCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("namespace", completeKubeNamespaceFlag)
}

func registerPullPolicyFlagCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("pull-policy", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeFixedValues(toComplete, []completionChoice{
			{value: "Always", desc: "Always pull the debug image"},
			{value: "IfNotPresent", desc: "Pull only when the debug image is missing"},
			{value: "Never", desc: "Never pull the debug image"},
		})
	})
}

func registerProfileFlagCompletion(cmd *cobra.Command) {
	_ = cmd.RegisterFlagCompletionFunc("profile", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		choices := make([]completionChoice, 0, len(runtime.ValidProfiles))
		for _, profile := range runtime.ValidProfiles {
			choices = append(choices, completionChoice{value: profile, desc: kubernetesProfileDescription(profile)})
		}
		return completeFixedValues(toComplete, choices)
	})
}

func kubernetesProfileDescription(profile string) string {
	switch profile {
	case runtime.ProfileGeneral:
		return "Default root debugging profile"
	case runtime.ProfileBaseline:
		return "PodSecurity baseline-compatible profile"
	case runtime.ProfileRestricted:
		return "Non-root restricted profile"
	case runtime.ProfileNetadmin:
		return "Network debugging capabilities"
	case runtime.ProfileSysadmin:
		return "Privileged/sysadmin profile"
	default:
		return "Kubernetes debug security profile"
	}
}

func completeTargetArg(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		// After the target, debux accepts an arbitrary command. Let the shell fall
		// back to its normal command/file completion instead of suggesting targets.
		return nil, cobra.ShellCompDirectiveDefault
	}
	if strings.HasPrefix(toComplete, "-") {
		return nil, cobra.ShellCompDirectiveDefault
	}
	return completeRuntimeTarget(cmd, toComplete)
}

func completeRuntimeTarget(cmd *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch {
	case strings.HasPrefix(toComplete, "k8s://"):
		return completeKubernetesTarget(cmd, toComplete)
	case strings.HasPrefix(toComplete, "docker://"):
		return completeDockerTarget(cmd, toComplete)
	case strings.Contains(toComplete, "://"):
		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	if strings.HasPrefix("docker://", toComplete) {
		completions = appendCompletion(completions, "docker://", "Docker container picker", toComplete)
	}
	if strings.HasPrefix("k8s://", toComplete) {
		completions = appendCompletion(completions, "k8s://", "Kubernetes pod picker", toComplete)
	}

	// The historical shorthand `debux <container>` remains first-class. Complete
	// running Docker container names without requiring users to type docker://,
	// but avoid probing Docker while the user is clearly typing a URI scheme.
	completePlainDocker := toComplete == "" || (!strings.HasPrefix("docker://", toComplete) && !strings.HasPrefix("k8s://", toComplete))
	if completePlainDocker {
		containers, err := listDockerContainersForCompletion()
		if err != nil {
			completions = appendActiveHelp(completions, "Docker completion unavailable: "+err.Error())
		} else {
			for _, c := range containers {
				completions = appendCompletion(completions, c.Name, dockerContainerCompletionDescription(c), toComplete)
				if toComplete != "" && strings.HasPrefix(c.ID, toComplete) {
					completions = appendCompletion(completions, c.ID, dockerContainerIDCompletionDescription(c), toComplete)
				}
			}
		}
	}

	completions = uniqueSortedCompletions(completions)
	if cmd.HasSubCommands() {
		// The root command also shows subcommands. A global NoSpace directive would
		// make completing `debux kill` or `debux docs` awkward, so only subcommands
		// like `debux exec k<TAB>` get no-space URI branch completion.
		return completions, cobra.ShellCompDirectiveNoFileComp
	}
	return completions, branchAwareDirective(completions)
}

func completeDockerTarget(_ *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	prefix := strings.TrimPrefix(toComplete, "docker://")
	containers, err := listDockerContainersForCompletion()
	if err != nil {
		return appendActiveHelp(nil, "Docker completion unavailable: "+err.Error()), cobra.ShellCompDirectiveNoFileComp
	}

	var completions []string
	for _, c := range containers {
		completions = appendCompletion(completions, "docker://"+c.Name, dockerContainerCompletionDescription(c), toComplete)
		if prefix != "" && strings.HasPrefix(c.ID, prefix) {
			completions = appendCompletion(completions, "docker://"+c.ID, dockerContainerIDCompletionDescription(c), toComplete)
		}
	}
	if len(completions) == 0 && prefix == "" {
		completions = appendActiveHelp(completions, "docker:// opens the Docker picker; no running containers were found for live completion")
	}
	return uniqueSortedCompletions(completions), cobra.ShellCompDirectiveNoFileComp
}

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

func completeDockerImages(_ *cobra.Command, toComplete string) ([]string, cobra.ShellCompDirective) {
	return completeDockerImageRefs(toComplete, true)
}

func completeDockerImageRefs(toComplete string, includeDefaultDebugImage bool) ([]string, cobra.ShellCompDirective) {
	images, err := listDockerImagesForCompletion()
	var completions []string
	if includeDefaultDebugImage {
		completions = appendCompletion(completions, runtime.DefaultImage, "Default debux debug image", toComplete)
	}
	if err != nil {
		return appendActiveHelp(completions, "Docker image completion unavailable: "+err.Error()), cobra.ShellCompDirectiveNoFileComp
	}
	for _, image := range images {
		desc := "Docker image"
		if image.ID != "" {
			desc += " · " + image.ID
		}
		if image.Containers > 0 {
			desc += fmt.Sprintf(" · used by %d container(s)", image.Containers)
		}
		completions = appendCompletion(completions, image.Ref, desc, toComplete)
	}
	return uniqueSortedCompletions(completions), cobra.ShellCompDirectiveNoFileComp
}

type completionChoice struct {
	value string
	desc  string
}

func completeFixedValues(toComplete string, choices []completionChoice) ([]string, cobra.ShellCompDirective) {
	var completions []string
	for _, choice := range choices {
		completions = appendCompletion(completions, choice.value, choice.desc, toComplete)
	}
	return completions, cobra.ShellCompDirectiveNoFileComp
}

func listDockerContainersForCompletion() ([]runtime.ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()

	containers, err := runtime.DockerList(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(containers, func(i, j int) bool {
		if containers[i].HasDebuxSession != containers[j].HasDebuxSession {
			return containers[i].HasDebuxSession
		}
		return containers[i].Name < containers[j].Name
	})
	return containers, nil
}

func listDockerImagesForCompletion() ([]runtime.ImageInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), completionTimeout)
	defer cancel()

	images, err := runtime.DockerImages(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(images, func(i, j int) bool { return images[i].Ref < images[j].Ref })
	if len(images) > completionMaxResults {
		images = images[:completionMaxResults]
	}
	return images, nil
}

func dockerContainerCompletionDescription(c runtime.ContainerInfo) string {
	desc := "Docker container"
	if c.Image != "" {
		desc += " · " + c.Image
	}
	if c.Status != "" {
		desc += " · " + c.Status
	}
	if c.HasDebuxSession {
		desc += " · active debux session"
	}
	return desc
}

func dockerContainerIDCompletionDescription(c runtime.ContainerInfo) string {
	desc := "Docker container ID"
	if c.Name != "" {
		desc += " · " + c.Name
	}
	if c.Image != "" {
		desc += " · " + c.Image
	}
	return desc
}

func completionFlagString(cmd *cobra.Command, name string) string {
	if cmd == nil || cmd.Flags().Lookup(name) == nil {
		return ""
	}
	value, _ := cmd.Flags().GetString(name)
	return value
}

func completionSelectedKubeContext(cmd *cobra.Command) string {
	return completionFlagString(cmd, "context")
}

func completionSelectedNamespace(cmd *cobra.Command, kubeconfig, kubeContext string) string {
	if completionNamespaceFlagChanged(cmd) {
		return completionFlagString(cmd, "namespace")
	}
	return runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext)
}

func completionNamespaceFlagChanged(cmd *cobra.Command) bool {
	flag := cmd.Flags().Lookup("namespace")
	return flag != nil && flag.Changed
}

func unescapeCompletionPart(part string) string {
	unescaped, err := url.PathUnescape(part)
	if err != nil {
		return part
	}
	return unescaped
}

func appendCompletion(completions []string, value, desc, toComplete string) []string {
	if toComplete != "" && !strings.HasPrefix(value, toComplete) {
		return completions
	}
	if desc == "" {
		return append(completions, value)
	}
	return append(completions, value+"\t"+desc)
}

func appendActiveHelp(completions []string, message string) []string {
	if strings.TrimSpace(message) == "" {
		return completions
	}
	return cobra.AppendActiveHelp(completions, message)
}

func uniqueCompletions(completions []string) []string {
	seen := make(map[string]struct{}, len(completions))
	out := make([]string, 0, len(completions))
	for _, completion := range completions {
		key := completionValue(completion)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, completion)
	}
	return out
}

func uniqueSortedCompletions(completions []string) []string {
	seen := make(map[string]struct{}, len(completions))
	out := make([]string, 0, len(completions))
	for _, completion := range completions {
		key := completionValue(completion)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, completion)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return completionValue(out[i]) < completionValue(out[j])
	})
	return out
}

func completionValue(completion string) string {
	value, _, _ := strings.Cut(completion, "\t")
	return value
}

func branchAwareDirective(completions []string) cobra.ShellCompDirective {
	directive := cobra.ShellCompDirectiveNoFileComp
	if len(completions) == 0 {
		return directive
	}
	sawCompletion := false
	for _, completion := range completions {
		value := completionValue(completion)
		if strings.HasPrefix(value, "_activeHelp_ ") {
			continue
		}
		sawCompletion = true
		if !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, "://") {
			return directive
		}
	}
	if !sawCompletion {
		return directive
	}
	return directive | cobra.ShellCompDirectiveNoSpace
}
