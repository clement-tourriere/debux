package cli

import (
	"reflect"
	"testing"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

func TestFormatTargetURIKubernetesContextNamespaceContainer(t *testing.T) {
	target := &runtime.Target{
		Runtime:   "kubernetes",
		Context:   "arn:aws:eks:us-west-2:123:cluster/preprod",
		Namespace: "prod",
		Name:      "api/pod",
		Container: "app",
	}

	got := formatTargetURI(target)
	want := "k8s://@arn:aws:eks:us-west-2:123:cluster%2Fpreprod/prod/api%2Fpod/app"
	if got != want {
		t.Fatalf("formatTargetURI() = %q, want %q", got, want)
	}
}

func TestBuildExecArgsDoesNotAddKubernetesOnlyFlagsForDocker(t *testing.T) {
	oldContext, oldNamespace := flagKubeContext, flagNamespace
	flagKubeContext, flagNamespace = "prod-context", "prod"
	t.Cleanup(func() { flagKubeContext, flagNamespace = oldContext, oldNamespace })

	cmd := &cobra.Command{}
	cmd.Flags().String("kubeconfig", "/tmp/kubeconfig", "")

	got := buildExecArgs(cmd, tuiLaunch{
		target:          "docker://app",
		copy:            true,
		profile:         runtime.ProfileRestricted,
		shareVolumes:    true,
		readOnlyVolumes: false,
	})
	want := []string{"exec", "docker://app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("buildExecArgs() = %#v, want %#v", got, want)
	}
}

func TestTUISourceItemsIncludeCurrentNamespaceShortcut(t *testing.T) {
	launch := tuiLaunch{shareVolumes: true, profile: runtime.ProfileGeneral}
	m := newTUIModel(&launch, "", "", "")
	m.contextItems = []tuiItem{
		{kind: tuiItemKubeContext, source: tuiSourceK8s, title: "prod", context: "prod", namespace: "gim", active: true},
	}
	m.dockerItems = []tuiItem{{kind: tuiItemTarget, source: tuiSourceDocker, title: "api"}}
	m.historyItems = []tuiItem{{kind: tuiItemTarget, source: tuiSourceHistory, title: "docker://api"}}

	items := m.sourceItems()
	if len(items) != 4 {
		t.Fatalf("len(sourceItems) = %d, want 4", len(items))
	}
	if items[0].kind != tuiItemSource || items[0].view != tuiViewDocker {
		t.Fatalf("first source item = %#v, want Docker source", items[0])
	}
	if items[2].kind != tuiItemKubeNamespace || items[2].context != "prod" || items[2].namespace != "gim" {
		t.Fatalf("current namespace shortcut = %#v", items[2])
	}
}

func TestTUICycleSource(t *testing.T) {
	launch := tuiLaunch{shareVolumes: true, profile: runtime.ProfileGeneral}
	m := newTUIModel(&launch, "", "", "")
	m.cycleSource(1)
	if m.view != tuiViewDocker {
		t.Fatalf("view after cycle = %q, want %q", m.view, tuiViewDocker)
	}
}

func TestTerminalArgs(t *testing.T) {
	cmdline := "debux exec docker://app"
	tests := []struct {
		name string
		want []string
	}{
		{name: "ghostty", want: []string{"-e", "sh", "-lc", cmdline}},
		{name: "wezterm", want: []string{"start", "--", "sh", "-lc", cmdline}},
		{name: "kitty", want: []string{"sh", "-lc", cmdline}},
		{name: "gnome-terminal", want: []string{"--", "sh", "-lc", cmdline}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := terminalArgs(tt.name, nil, cmdline)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("terminalArgs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
