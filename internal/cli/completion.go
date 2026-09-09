package cli

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

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
	// The constant is intentionally the haystack: suggest a scheme while the
	// user has typed only its prefix.
	if strings.HasPrefix("docker://", toComplete) { //nolint:gocritic // see above
		completions = appendCompletion(completions, "docker://", "Docker container picker", toComplete)
	}
	if strings.HasPrefix("k8s://", toComplete) { //nolint:gocritic // scheme-prefix check
		completions = appendCompletion(completions, "k8s://", "Kubernetes pod picker", toComplete)
	}

	// The historical shorthand `debux <container>` remains first-class. Complete
	// running Docker container names without requiring users to type docker://,
	// but avoid probing Docker while the user is clearly typing a URI scheme.
	completePlainDocker := toComplete == "" || (!strings.HasPrefix("docker://", toComplete) && !strings.HasPrefix("k8s://", toComplete)) //nolint:gocritic // scheme-prefix checks
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

	containers, err := runtime.DockerList(ctx, nil)
	if err != nil {
		return nil, err
	}
	// No sessions-first ordering here: uniqueSortedCompletions re-sorts
	// completions alphabetically, so only the name order survives.
	sort.SliceStable(containers, func(i, j int) bool {
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
