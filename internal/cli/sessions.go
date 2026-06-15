package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/clement-tourriere/debux/internal/history"
	"github.com/clement-tourriere/debux/internal/picker"
	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

var flagAllNamespaces bool

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list [target-scope]",
		Aliases: []string{"ls", "sessions"},
		Short:   "List running debux sessions",
		Long: `List running debux sessions that can be reattached.

With no target, debux lists Docker sidecars plus Kubernetes sessions in the
current namespace and recent Kubernetes namespaces from debux history. Pass a
Kubernetes scope or flags to focus the cluster lookup.`,
		Example: `  debux list
  debux ls k8s://@eks-preprod-01/gim/
  debux sessions --context eks-preprod-01 --namespace gim
  debux list k8s://@eks-preprod-01 --all-namespaces

  # Reattach to any TARGET shown by the list
  debux attach k8s://@eks-preprod-01/gim/debux-copy-abc12`,
		Args:          cobra.MaximumNArgs(1),
		RunE:          runList,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addKubernetesFlags(cmd)
	cmd.Flags().BoolVarP(&flagAllNamespaces, "all-namespaces", "A", false, "Kubernetes: list sessions across all namespaces")
	configureTargetCompletion(cmd)
	return cmd
}

func newAttachCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "attach [target]",
		Aliases: []string{"reattach"},
		Short:   "Reattach to a running debux session",
		Long: `Reattach to a running debux session.

With no target, debux opens a searchable picker over active Docker sessions plus
Kubernetes sessions in the current namespace and recent Kubernetes namespaces
from debux history. Type to filter by namespace, pod, target URI, or source;
selecting a session reuses the exact debug image, user, and profile recorded on
that session when available.`,
		Example: `  debux attach
  debux attach k8s://@eks-preprod-01
  debux attach --context eks-preprod-01 --namespace gim
  debux attach k8s://@eks-preprod-01/gim/debux-copy-abc12
  debux reattach docker://my-app`,
		Args:          cobra.MaximumNArgs(1),
		RunE:          runAttach,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	addExecFlags(cmd)
	cmd.Flags().BoolVarP(&flagAllNamespaces, "all-namespaces", "A", false, "Kubernetes: include sessions across all namespaces in the picker")
	configureTargetCompletion(cmd)
	return cmd
}

func runList(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	rt, kubeContext, namespace, nameFilter, err := sessionScope(cmd, args)
	if err != nil {
		return err
	}
	if flagAllNamespaces && namespace != "" {
		return fmt.Errorf("--all-namespaces cannot be combined with namespace %q", namespace)
	}

	sessions, problems := collectDebugSessions(ctx, cmd, rt, kubeContext, namespace, flagAllNamespaces)
	sessions = filterDebugSessions(sessions, nameFilter)
	if len(sessions) == 0 {
		if len(problems) > 0 {
			return fmt.Errorf("no running debux sessions found, but some runtimes could not be checked:\n  %s", strings.Join(errorStrings(problems), "\n  "))
		}
		fmt.Println("No running debux sessions found")
		return nil
	}

	printDebugSessions(sessions)
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "Warning: %v\n", problem)
	}
	return nil
}

func runAttach(cmd *cobra.Command, args []string) error {
	ctx, cancel := signalContext()
	defer cancel()

	if len(args) > 0 {
		return attachExplicitTarget(ctx, cmd, args[0])
	}

	rt := ""
	if flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace") || flagAllNamespaces {
		rt = "kubernetes"
	}
	if flagAllNamespaces && flagNamespace != "" {
		return fmt.Errorf("--all-namespaces cannot be combined with namespace %q", flagNamespace)
	}

	return attachFromPicker(ctx, cmd, rt, flagKubeContext, flagNamespace, flagAllNamespaces, "Select a debux session to reattach (type to search)")
}

func attachExplicitTarget(ctx context.Context, cmd *cobra.Command, rawTarget string) error {
	target, err := runtime.ParseTarget(rawTarget)
	if err != nil {
		return fmt.Errorf("invalid target: %w", err)
	}
	if target.Runtime != "docker" && target.Runtime != "kubernetes" {
		return fmt.Errorf("attach is not supported for runtime %q", target.Runtime)
	}
	if target.Runtime != "kubernetes" && (flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace")) {
		return fmt.Errorf("--context, --kubeconfig, and --namespace are only supported for Kubernetes targets; use k8s://... or remove the flag")
	}

	kubeContext := ""
	namespace := ""
	if target.Runtime == "kubernetes" {
		applyKubeNamespaceFlagContainerShorthand(cmd, target)
		kubeContext, err = resolveKubeContext(cmd, target.Context)
		if err != nil {
			return err
		}
		namespace, err = resolveKubeNamespace(cmd, target.Namespace)
		if err != nil {
			return err
		}
		if flagAllNamespaces && namespace != "" {
			return fmt.Errorf("--all-namespaces cannot be combined with namespace %q", namespace)
		}
	}
	if target.Name == "" {
		return attachFromPicker(ctx, cmd, target.Runtime, kubeContext, namespace, flagAllNamespaces, "Select a debux session to reattach (type to search)")
	}

	sessions, problems := collectDebugSessions(ctx, cmd, target.Runtime, kubeContext, namespace, false)
	matches := sessionsMatchingTarget(sessions, target, namespace)
	if len(matches) == 0 {
		if len(problems) > 0 {
			return fmt.Errorf("no running debux session found for %s, and some runtimes could not be checked:\n  %s", rawTarget, strings.Join(errorStrings(problems), "\n  "))
		}
		return fmt.Errorf("no running debux session found for %s", rawTarget)
	}

	session := matches[0]
	if len(matches) > 1 {
		items := make([]picker.Item, len(matches))
		for i, match := range matches {
			items[i] = picker.Item{Label: formatDebugSessionLabel(match), Value: strconv.Itoa(i)}
		}
		chosen, err := picker.Pick("Select a matching debux session to reattach (type to search)", items)
		if err != nil {
			return err
		}
		idx, err := strconv.Atoi(chosen)
		if err != nil || idx < 0 || idx >= len(matches) {
			return fmt.Errorf("invalid session selection %q", chosen)
		}
		session = matches[idx]
	}

	applySessionLaunchFlags(cmd, session)
	return runExec(cmd, []string{session.Target})
}

func attachFromPicker(ctx context.Context, cmd *cobra.Command, rt, kubeContext, namespace string, allNamespaces bool, title string) error {
	sessions, problems := collectDebugSessions(ctx, cmd, rt, kubeContext, namespace, allNamespaces)
	if len(sessions) == 0 {
		if len(problems) > 0 {
			return fmt.Errorf("no running debux sessions found, but some runtimes could not be checked:\n  %s", strings.Join(errorStrings(problems), "\n  "))
		}
		return fmt.Errorf("no running debux sessions found")
	}

	items := make([]picker.Item, len(sessions))
	for i, session := range sessions {
		items[i] = picker.Item{Label: formatDebugSessionLabel(session), Value: strconv.Itoa(i)}
	}
	chosen, err := picker.Pick(title, items)
	if err != nil {
		return err
	}
	idx, err := strconv.Atoi(chosen)
	if err != nil || idx < 0 || idx >= len(sessions) {
		return fmt.Errorf("invalid session selection %q", chosen)
	}

	applySessionLaunchFlags(cmd, sessions[idx])
	return runExec(cmd, []string{sessions[idx].Target})
}

func sessionsMatchingTarget(sessions []runtime.DebugSessionInfo, target *runtime.Target, namespace string) []runtime.DebugSessionInfo {
	var matches []runtime.DebugSessionInfo
	for _, session := range sessions {
		if session.Runtime != target.Runtime {
			continue
		}
		switch target.Runtime {
		case "docker":
			if session.Name == target.Name || session.Target == "docker://"+target.Name {
				matches = append(matches, session)
			}
		case "kubernetes":
			if namespace != "" && session.Namespace != namespace {
				continue
			}
			if session.Name != target.Name {
				continue
			}
			if target.Container != "" && session.TargetContainer != target.Container {
				continue
			}
			matches = append(matches, session)
		}
	}
	return matches
}

func sessionScope(cmd *cobra.Command, args []string) (rt, kubeContext, namespace, nameFilter string, err error) {
	rt = ""
	kubernetesFlagsSet := flagChanged(cmd, "context") || flagChanged(cmd, "kubeconfig") || flagChanged(cmd, "namespace")
	if kubernetesFlagsSet || flagAllNamespaces {
		rt = "kubernetes"
		kubeContext = flagKubeContext
		namespace = flagNamespace
	}
	if len(args) == 0 {
		return rt, kubeContext, namespace, "", nil
	}

	target, err := runtime.ParseTarget(args[0])
	if err != nil {
		return "", "", "", "", fmt.Errorf("invalid target: %w", err)
	}
	if target.Runtime != "docker" && target.Runtime != "kubernetes" {
		return "", "", "", "", fmt.Errorf("listing sessions is not supported for runtime %q", target.Runtime)
	}
	if target.Runtime != "kubernetes" && kubernetesFlagsSet {
		return "", "", "", "", fmt.Errorf("--context, --kubeconfig, and --namespace are only supported for Kubernetes targets; use k8s://... or remove the flag")
	}

	rt = target.Runtime
	nameFilter = target.Name
	if rt == "kubernetes" {
		applyKubeNamespaceFlagContainerShorthand(cmd, target)
		kubeContext, err = resolveKubeContext(cmd, target.Context)
		if err != nil {
			return "", "", "", "", err
		}
		namespace, err = resolveKubeNamespace(cmd, target.Namespace)
		if err != nil {
			return "", "", "", "", err
		}
	}
	return rt, kubeContext, namespace, nameFilter, nil
}

func collectDebugSessions(ctx context.Context, cmd *cobra.Command, rt, kubeContext, namespace string, allNamespaces bool) ([]runtime.DebugSessionInfo, []error) {
	var sessions []runtime.DebugSessionInfo
	var problems []error

	if rt == "" || rt == "docker" {
		items, err := runtime.DockerSessions(ctx)
		if err != nil {
			problems = append(problems, fmt.Errorf("docker: %w", err))
		} else {
			sessions = append(sessions, items...)
		}
	}
	if rt == "" || rt == "kubernetes" {
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		scopes := kubernetesSessionScopes(kubeconfig, kubeContext, namespace, !allNamespaces)
		for _, scope := range scopes {
			items, err := runtime.KubernetesSessions(ctx, kubeconfig, scope.context, scope.namespace, allNamespaces)
			if err != nil {
				problems = append(problems, fmt.Errorf("kubernetes %s: %w", scope.label(), err))
			} else {
				sessions = append(sessions, items...)
			}
		}
	}
	if rt != "" && rt != "docker" && rt != "kubernetes" {
		problems = append(problems, fmt.Errorf("unsupported runtime %q", rt))
	}

	sessions = dedupeDebugSessions(sessions)
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Runtime != sessions[j].Runtime {
			return sessions[i].Runtime < sessions[j].Runtime
		}
		return sessions[i].Target < sessions[j].Target
	})
	return sessions, problems
}

const maxDefaultKubernetesSessionScopes = 8

type kubernetesSessionScope struct {
	context   string
	namespace string
}

func (s kubernetesSessionScope) label() string {
	parts := make([]string, 0, 2)
	if s.context != "" {
		parts = append(parts, s.context)
	} else {
		parts = append(parts, "current-context")
	}
	if s.namespace != "" {
		parts = append(parts, s.namespace)
	}
	return strings.Join(parts, "/")
}

func kubernetesSessionScopes(kubeconfig, kubeContext, namespace string, includeHistory bool) []kubernetesSessionScope {
	var scopes []kubernetesSessionScope
	seen := make(map[string]struct{})
	add := func(context, ns string) {
		key := context + "\x00" + ns
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		scopes = append(scopes, kubernetesSessionScope{context: context, namespace: ns})
	}

	if namespace != "" {
		add(kubeContext, namespace)
		return scopes
	}

	add(kubeContext, runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext))
	if !includeHistory {
		return scopes
	}

	entries, err := history.Load()
	if err != nil {
		return scopes
	}
	for _, entry := range entries {
		if len(scopes) >= maxDefaultKubernetesSessionScopes {
			break
		}
		if entry.Runtime != "kubernetes" || entry.Namespace == "" {
			continue
		}
		if kubeContext != "" && entry.Context != kubeContext {
			continue
		}
		add(entry.Context, entry.Namespace)
	}
	return scopes
}

func dedupeDebugSessions(sessions []runtime.DebugSessionInfo) []runtime.DebugSessionInfo {
	seen := make(map[string]struct{}, len(sessions))
	out := make([]runtime.DebugSessionInfo, 0, len(sessions))
	for _, session := range sessions {
		key := strings.Join([]string{session.Runtime, session.Kind, session.Target, session.DebugName}, "\x00")
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, session)
	}
	return out
}

func filterDebugSessions(sessions []runtime.DebugSessionInfo, name string) []runtime.DebugSessionInfo {
	if name == "" {
		return sessions
	}
	var filtered []runtime.DebugSessionInfo
	for _, session := range sessions {
		if session.Name == name || session.Source == name || strings.Contains(session.Target, "/"+name) || strings.Contains(session.Target, "/"+name+"/") {
			filtered = append(filtered, session)
		}
	}
	return filtered
}

func printDebugSessions(sessions []runtime.DebugSessionInfo) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "RUNTIME\tKIND\tTARGET\tDEBUG\tSTATUS\tREATTACH")
	for _, session := range sessions {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\tdebux attach %s\n",
			session.Runtime,
			shortDebugSessionKind(session.Kind),
			session.Target,
			emptyAs(session.DebugName, "-"),
			debugSessionStatus(session),
			session.Target,
		)
	}
	_ = tw.Flush()
}

func formatDebugSessionLabel(session runtime.DebugSessionInfo) string {
	return fmt.Sprintf("● %s · %s · %s · %s", session.Target, shortDebugSessionKind(session.Kind), emptyAs(session.DebugName, "-"), debugSessionStatus(session))
}

func shortDebugSessionKind(kind string) string {
	switch kind {
	case runtime.DebugSessionKindDockerSidecar:
		return "sidecar"
	case runtime.DebugSessionKindKubernetesEphemeral:
		return "ephemeral"
	case runtime.DebugSessionKindKubernetesCopyPod:
		return "copy-pod"
	default:
		return kind
	}
}

func debugSessionStatus(session runtime.DebugSessionInfo) string {
	var parts []string
	if session.Status != "" {
		parts = append(parts, session.Status)
	}
	if !session.StartedAt.IsZero() {
		parts = append(parts, "started "+humanDuration(time.Since(session.StartedAt))+" ago")
	}
	if session.HasExpiry {
		parts = append(parts, "expires in "+humanDuration(session.ExpiresIn))
	}
	if len(parts) == 0 {
		return "running"
	}
	return strings.Join(parts, "; ")
}

func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d >= time.Hour {
		return d.Truncate(time.Minute).String()
	}
	if d >= time.Minute {
		return d.Truncate(time.Second).String()
	}
	return d.Truncate(time.Second).String()
}

func errorStrings(errs []error) []string {
	out := make([]string, 0, len(errs))
	for _, err := range errs {
		out = append(out, err.Error())
	}
	return out
}

func applySessionLaunchFlags(cmd *cobra.Command, session runtime.DebugSessionInfo) {
	flagFresh = false
	flagCopy = false
	clearFlagValue(cmd, "fresh", "false")
	clearFlagValue(cmd, "copy", "false")
	clearFlagValue(cmd, "keep", "false")
	clearFlagValue(cmd, "ttl", defaultCopyPodTTL)
	if session.Image != "" {
		flagImage = session.Image
		_ = cmd.Flags().Set("image", session.Image)
	}
	flagUser = session.User
	_ = cmd.Flags().Set("user", session.User)
	if session.Runtime == "kubernetes" {
		profile := session.Profile
		if profile == "" {
			profile = runtime.ProfileGeneral
		}
		flagProfile = profile
		_ = cmd.Flags().Set("profile", profile)
	}
}

func clearFlagValue(cmd *cobra.Command, name, value string) {
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return
	}
	_ = flag.Value.Set(value)
	flag.Changed = false
}
