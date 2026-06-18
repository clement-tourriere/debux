package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	got, err := debugExtraEnv([]string{"FOO=bar"}, []string{"py-spy", "gdb"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "FOO=bar" || got[1] != "DEBUX_TOOLS=py-spy gdb" {
		t.Fatalf("unexpected env: %v", got)
	}
	extra, err := debugExtraEnv(nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extra) != 0 {
		t.Fatalf("expected no entries, got %v", extra)
	}
}

func TestValidateTools(t *testing.T) {
	if err := ValidateTools([]string{"python3", "py-spy", "nixpkgs#hello", "github:owner/repo#pkg"}); err != nil {
		t.Fatalf("valid tools rejected: %v", err)
	}
	for _, bad := range []string{"foo;bar", "foo bar", "foo|bar", "foo$(x)", "foo`bar", "foo*", "foo\nbar", ""} {
		if err := ValidateTools([]string{bad}); err == nil {
			t.Errorf("ValidateTools(%q): expected error", bad)
		}
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
	// Required guards: the pidfile, a PID-1 exclusion in the scan, a final
	// PID-1/empty guard before kill, and cmdline-based matching (not the
	// DEBUX_DAEMON env marker, which the sleep child inherits).
	for _, marker := range []string{
		"/tmp/.debux-daemon.pid",
		`[ "$p" = "1" ] && continue`,
		"DEBUX_TARGET_ROOT",
		`/proc/[0-9]*`,
	} {
		if !strings.Contains(killDebuxDaemonScript, marker) {
			t.Fatalf("kill script is missing %q", marker)
		}
	}
	// Must not match the daemon by an env marker the sleep child inherits.
	if strings.Contains(killDebuxDaemonScript, "DEBUX_DAEMON=1") {
		t.Fatal("kill script must not match the daemon by DEBUX_DAEMON=1 (inherited by the sleep child)")
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

func TestBuildKubernetesCopyPodLifecycle(t *testing.T) {
	source := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api-1", Namespace: "prod"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "app"}},
		},
	}

	opts := DebugOpts{Image: "img:v1", Profile: ProfileGeneral, TTL: 2 * time.Hour}
	pod, debugName, err := buildKubernetesCopyPod("prod", source, "app", opts, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if debugName != "debux" {
		t.Fatalf("debug container name = %q, want debux", debugName)
	}
	if pod.Spec.NodeName != "" {
		t.Fatal("copy pod must not be pinned to the source node")
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds != 7200 {
		t.Fatalf("TTL must map to activeDeadlineSeconds, got %v", pod.Spec.ActiveDeadlineSeconds)
	}
	if got := pod.Annotations[karpenterDoNotDisruptAnnotation]; got != "true" {
		t.Fatalf("karpenter protection must use boolean true, got %q", got)
	}
	if got := pod.Annotations[karpenterDoNotEvictAnnotation]; got != "true" {
		t.Fatalf("legacy karpenter do-not-evict annotation = %q, want true", got)
	}
	if got := pod.Annotations[karpenterDoNotConsolidateAnnotation]; got != "true" {
		t.Fatalf("legacy karpenter do-not-consolidate annotation = %q, want true", got)
	}
	if got := pod.Annotations[clusterAutoscalerSafeToEvictAnnotation]; got != "false" {
		t.Fatalf("cluster-autoscaler annotation = %q, want false", got)
	}
	if got := pod.Annotations[debuxTargetContainerAnnotation]; got != "app" {
		t.Fatalf("target-container annotation = %q, want app (reattach depends on it)", got)
	}
	if got := pod.Annotations[debuxSourcePodAnnotation]; got != "api-1" {
		t.Fatalf("source-pod annotation = %q, want api-1", got)
	}
	if !isKubernetesCopyPod(pod) {
		t.Fatal("built copy pod must be recognized by reattach/kill detection")
	}
	if got := findCopyPodDebugContainer(pod); got != "debux" {
		t.Fatalf("findCopyPodDebugContainer = %q, want debux", got)
	}

	// TTL=0 means the user explicitly opted out: no deadline (not even an
	// inherited one) and unbounded karpenter protection.
	src2 := source.DeepCopy()
	var inherited int64 = 30
	src2.Spec.ActiveDeadlineSeconds = &inherited
	pod, _, err = buildKubernetesCopyPod("prod", src2, "app", DebugOpts{Image: "img:v1", Profile: ProfileGeneral}, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.ActiveDeadlineSeconds != nil {
		t.Fatal("ttl=0 must clear the deadline, including one inherited from the source spec")
	}
	if got := pod.Annotations[karpenterDoNotDisruptAnnotation]; got != "true" {
		t.Fatalf("without a TTL karpenter protection must still be enabled, got %q", got)
	}
	if got := pod.Annotations[karpenterDoNotEvictAnnotation]; got != "true" {
		t.Fatalf("legacy karpenter do-not-evict annotation = %q, want true", got)
	}
	if got := pod.Annotations[karpenterDoNotConsolidateAnnotation]; got != "true" {
		t.Fatalf("legacy karpenter do-not-consolidate annotation = %q, want true", got)
	}

	// A source container already named debux must not collide with the debug
	// container, and the daemon marker must still identify the right one.
	src3 := source.DeepCopy()
	src3.Spec.Containers = []corev1.Container{{Name: "app"}, {Name: "debux"}}
	pod, debugName, err = buildKubernetesCopyPod("prod", src3, "app", opts, "ctx")
	if err != nil {
		t.Fatal(err)
	}
	if debugName != "debux-1" {
		t.Fatalf("debug container name = %q, want debux-1", debugName)
	}
	if got := findCopyPodDebugContainer(pod); got != "debux-1" {
		t.Fatalf("findCopyPodDebugContainer must follow the DEBUX_DAEMON marker, got %q", got)
	}
}

func TestIsKubernetesCopyPodIgnoresUserPods(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Labels: map[string]string{debuxManagedByLabelKey: debuxManagedByLabelValue},
	}}
	if isKubernetesCopyPod(pod) {
		t.Fatal("managed-by alone (e.g. node debug pods) must not be treated as a copy pod")
	}
	pod.Labels[debuxModeLabelKey] = debuxModeCopy
	if !isKubernetesCopyPod(pod) {
		t.Fatal("copy-labeled pod must be detected")
	}
	if isKubernetesCopyPod(&corev1.Pod{}) {
		t.Fatal("unlabeled pod must not be treated as a copy pod")
	}
}

func TestKubernetesSessionsForPod(t *testing.T) {
	started := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "prod"},
		Spec: corev1.PodSpec{EphemeralContainers: []corev1.EphemeralContainer{{
			EphemeralContainerCommon: corev1.EphemeralContainerCommon{
				Name:  "debux-123",
				Image: "debug:v1",
				Env: []corev1.EnvVar{
					{Name: "DEBUX_TARGET", Value: "ctx:prod/api/app"},
					{Name: "DEBUX_SECURITY_PROFILE", Value: ProfileRestricted},
					{Name: "DEBUX_DEBUG_USER", Value: "1000:1000"},
				},
			},
			TargetContainerName: "app",
		}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			EphemeralContainerStatuses: []corev1.ContainerStatus{{
				Name:  "debux-123",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			}},
		},
	}

	got := kubernetesSessionsForPod(pod, "ctx")
	if len(got) != 1 {
		t.Fatalf("sessions = %#v, want one", got)
	}
	if got[0].Kind != DebugSessionKindKubernetesEphemeral || got[0].Target != "k8s://@ctx/prod/api/app" {
		t.Fatalf("ephemeral session = %#v", got[0])
	}
	if got[0].Profile != ProfileRestricted || got[0].User != "1000:1000" || got[0].Image != "debug:v1" {
		t.Fatalf("ephemeral session metadata = %#v", got[0])
	}

	deadline := int64(3600)
	copyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "debux-copy-abc12",
			Namespace: "prod",
			Labels: map[string]string{
				debuxManagedByLabelKey: debuxManagedByLabelValue,
				debuxModeLabelKey:      debuxModeCopy,
			},
			Annotations: map[string]string{
				debuxSourcePodAnnotation:       "api",
				debuxTargetContainerAnnotation: "app",
			},
		},
		Spec: corev1.PodSpec{
			ActiveDeadlineSeconds: &deadline,
			Containers: []corev1.Container{{
				Name:  "debux",
				Image: "debug:v2",
				Env: []corev1.EnvVar{
					{Name: "DEBUX_DAEMON", Value: "1"},
					{Name: "DEBUX_SECURITY_PROFILE", Value: ProfileGeneral},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodRunning,
			StartTime: &started,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "debux",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}},
			}},
		},
	}
	got = kubernetesSessionsForPod(copyPod, "ctx")
	if len(got) != 1 {
		t.Fatalf("copy sessions = %#v, want one", got)
	}
	if got[0].Kind != DebugSessionKindKubernetesCopyPod || got[0].Target != "k8s://@ctx/prod/debux-copy-abc12" {
		t.Fatalf("copy session = %#v", got[0])
	}
	if got[0].Source != "api" || got[0].TargetContainer != "app" || !got[0].HasExpiry {
		t.Fatalf("copy session metadata = %#v", got[0])
	}
}
