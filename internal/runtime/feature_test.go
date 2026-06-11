package runtime

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
)

func TestParseTargetSchemes(t *testing.T) {
	tests := []struct {
		in      string
		want    Target
		wantErr bool
	}{
		{in: "my-app", want: Target{Runtime: "docker", Name: "my-app"}},
		{in: "docker://my-app", want: Target{Runtime: "docker", Name: "my-app"}},
		{in: "podman://my-app", want: Target{Runtime: "docker", Name: "my-app", PreferPodman: true}},
		{in: "compose://web", want: Target{Runtime: "docker", ComposeService: "web"}},
		{in: "compose://shop/web", want: Target{Runtime: "docker", ComposeProject: "shop", ComposeService: "web"}},
		{in: "compose://", wantErr: true},
		{in: "compose://a/b/c", wantErr: true},
		{in: "nerdctl://x", want: Target{Runtime: "containerd", Name: "x"}},
		{in: "containerd://x", want: Target{Runtime: "containerd", Name: "x"}},
		{in: "weird://x", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseTarget(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTarget(%q): expected error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTarget(%q): %v", tc.in, err)
			continue
		}
		if *got != tc.want {
			t.Errorf("ParseTarget(%q) = %+v, want %+v", tc.in, *got, tc.want)
		}
	}
}

func TestParsePortMappings(t *testing.T) {
	mappings, err := ParsePortMappings([]string{"80", "8080:81"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []PortMapping{{Local: 80, Remote: 80}, {Local: 8080, Remote: 81}}
	for i, m := range mappings {
		if m != want[i] {
			t.Errorf("mapping[%d] = %+v, want %+v", i, m, want[i])
		}
	}
	for _, bad := range []string{"0", "x", "70000", "8080:", ":80"} {
		if _, err := ParsePortMappings([]string{bad}); err == nil {
			t.Errorf("ParsePortMappings(%q): expected error", bad)
		}
	}
}

func TestNormalizeCapabilities(t *testing.T) {
	got := normalizeCapabilities([]string{"net_admin", "CAP_SYS_PTRACE", " ", "Bpf"})
	want := []string{"NET_ADMIN", "SYS_PTRACE", "BPF"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestValidateEnvVars(t *testing.T) {
	if err := ValidateEnvVars([]string{"FOO=bar", "A_B1=x", "_X=1=2"}); err != nil {
		t.Fatalf("valid env rejected: %v", err)
	}
	for _, bad := range []string{"FOO", "=v", "1AB=2", "A-B=x"} {
		if err := ValidateEnvVars([]string{bad}); err == nil {
			t.Errorf("ValidateEnvVars(%q): expected error", bad)
		}
	}
}

func TestDebugExtraEnv(t *testing.T) {
	got := debugExtraEnv([]string{"FOO=bar"}, []string{"py-spy", "gdb"})
	if len(got) != 2 || got[0] != "FOO=bar" || got[1] != "DEBUX_TOOLS=py-spy gdb" {
		t.Fatalf("unexpected env: %v", got)
	}
	if extra := debugExtraEnv(nil, nil); len(extra) != 0 {
		t.Fatalf("expected no entries, got %v", extra)
	}
}

func TestTargetMountsReservesShellStatePaths(t *testing.T) {
	info := container.InspectResponse{
		Mounts: []container.MountPoint{
			{Type: "volume", Name: "data", Destination: "/data", RW: true},
			{Type: "volume", Name: "tmp", Destination: "/tmp", RW: true},
			{Type: "volume", Name: "roothome", Destination: "/root", RW: true},
		},
	}
	mounts := targetMounts(info, false)
	if len(mounts) != 1 || mounts[0].Target != "/data" {
		t.Fatalf("expected only /data to be shared, got %+v", mounts)
	}

	// In copy mode the target root lives under /target, so /target/tmp is
	// data, not the debug shell's state — it must be mounted.
	prefixed := targetMountsAt(info, false, "/target")
	if len(prefixed) != 3 {
		t.Fatalf("expected all 3 mounts under /target, got %+v", prefixed)
	}
	for _, m := range prefixed {
		if !strings.HasPrefix(m.Target, "/target/") {
			t.Fatalf("mount not re-rooted: %+v", m)
		}
	}
}

func TestDockerDebugContainerImageMatches(t *testing.T) {
	withLabel := container.Summary{Labels: map[string]string{dockerLabelDebugImage: "img:v1"}}
	legacy := container.Summary{}

	if !dockerDebugContainerImageMatches(withLabel, "img:v1") {
		t.Error("same image should match")
	}
	if dockerDebugContainerImageMatches(withLabel, "img:v2") {
		t.Error("different image must not match")
	}
	if !dockerDebugContainerImageMatches(withLabel, dockerAnyDebugImage) {
		t.Error("wildcard should match")
	}
	if !dockerDebugContainerImageMatches(legacy, "img:v2") {
		t.Error("legacy sidecars without the label should match any image")
	}
}

func TestFindRunningDebuxContainerForTargetHonorsImage(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name:  "debux-1",
				Image: "img:v1",
				Env: []corev1.EnvVar{
					{Name: "DEBUX_TARGET", Value: "ns/pod/app"},
					{Name: "DEBUX_SECURITY_PROFILE", Value: ProfileGeneral},
				},
			},
			TargetContainerName: "app",
		}}},
		Status: corev1.PodStatus{EphemeralContainerStatuses: []corev1.ContainerStatus{
			{Name: "debux-1", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		}},
	}

	if got := findRunningDebuxContainerForTarget(pod, "app", ProfileGeneral, "", "img:v1"); got != "debux-1" {
		t.Fatalf("same image should reuse, got %q", got)
	}
	if got := findRunningDebuxContainerForTarget(pod, "app", ProfileGeneral, "", "img:v2"); got != "" {
		t.Fatalf("different image must not reuse, got %q", got)
	}
	if got := findRunningDebuxContainerForTarget(pod, "app", ProfileGeneral, "", ""); got != "debux-1" {
		t.Fatalf("empty image should match any, got %q", got)
	}
}

func TestSelectKubernetesCopyTargetContainerAllowsCrashLooping(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			{Name: "sidecar", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
		}},
	}

	got, err := selectKubernetesCopyTargetContainer(pod, "")
	if err != nil || got != "app" {
		t.Fatalf("copy mode should fall back to the first spec container, got %q (%v)", got, err)
	}
	got, err = selectKubernetesCopyTargetContainer(pod, "sidecar")
	if err != nil || got != "sidecar" {
		t.Fatalf("explicitly requested container should be accepted, got %q (%v)", got, err)
	}
	if _, err := selectKubernetesCopyTargetContainer(pod, "nope"); err == nil {
		t.Fatal("unknown container must error")
	}

	// The non-copy selector must still refuse the crash-looping pod.
	if _, err := selectKubernetesTargetContainer(pod, ""); err == nil {
		t.Fatal("live debugging a pod with no running containers must error")
	}
}

func TestKillScriptTargetsDaemonNotPid1(t *testing.T) {
	if strings.Contains(killDebuxDaemonScript, `kill 1`) && !strings.Contains(killDebuxDaemonScript, `kill "$pid"`) {
		t.Fatal("kill script must signal the daemon PID, never PID 1")
	}
	for _, marker := range []string{"/tmp/.debux-daemon.pid", "DEBUX_DAEMON=1", `[ "$p" = "1" ] && continue`} {
		if !strings.Contains(killDebuxDaemonScript, marker) {
			t.Fatalf("kill script is missing %q", marker)
		}
	}
}

func TestKubernetesTargetRootPath(t *testing.T) {
	if got := kubernetesTargetRootPath("/app/heap.prof"); got != "/proc/1/root/app/heap.prof" {
		t.Fatalf("got %q", got)
	}
	if got := kubernetesTargetRootPath("var/log"); got != "/proc/1/root/var/log" {
		t.Fatalf("got %q", got)
	}
	if got := kubernetesTargetRootPath("/a/../b"); got != "/proc/1/root/b" {
		t.Fatalf("got %q", got)
	}
}

func TestPodSidecarContainersAreTargetable(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init-once"},
				{Name: "proxy", RestartPolicy: &always},
			},
			Containers: []corev1.Container{{Name: "app"}},
		},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "proxy", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	if !podHasContainer(pod, "proxy") {
		t.Fatal("native sidecar should be targetable")
	}
	if podHasContainer(pod, "init-once") {
		t.Fatal("plain init container must not be targetable")
	}
	if !podContainerRunning(pod, "proxy") {
		t.Fatal("running sidecar should be reported running")
	}
	got, err := selectKubernetesTargetContainer(pod, "proxy")
	if err != nil || got != "proxy" {
		t.Fatalf("selecting the sidecar should work, got %q (%v)", got, err)
	}
}
