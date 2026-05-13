package runtime

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	corev1 "k8s.io/api/core/v1"
)

func TestParseK8sTargetNamespaceSemantics(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCtx   string
		wantNS    string
		wantName  string
		wantCtr   string
		wantError bool
	}{
		{name: "picker current namespace", raw: "k8s://"},
		{name: "pod current namespace", raw: "k8s://api", wantName: "api"},
		{name: "explicit default namespace", raw: "k8s://default/api", wantNS: "default", wantName: "api"},
		{name: "explicit namespace picker", raw: "k8s://prod/", wantNS: "prod"},
		{name: "explicit container", raw: "k8s://prod/api/app", wantNS: "prod", wantName: "api", wantCtr: "app"},
		{name: "context picker", raw: "k8s://@eks-preprod-01", wantCtx: "eks-preprod-01"},
		{name: "context pod current namespace", raw: "k8s://@eks-preprod-01/api", wantCtx: "eks-preprod-01", wantName: "api"},
		{name: "context namespace pod", raw: "k8s://@eks-preprod-01/prod/api", wantCtx: "eks-preprod-01", wantNS: "prod", wantName: "api"},
		{name: "context namespace pod container", raw: "k8s://@eks-preprod-01/prod/api/app", wantCtx: "eks-preprod-01", wantNS: "prod", wantName: "api", wantCtr: "app"},
		{name: "escaped context", raw: "k8s://@arn%3Aaws%3Aeks%3Aus-west-2%3A123%3Acluster%2Fprod/prod/api", wantCtx: "arn:aws:eks:us-west-2:123:cluster/prod", wantNS: "prod", wantName: "api"},
		{name: "empty context", raw: "k8s://@/prod/api", wantError: true},
		{name: "missing namespace", raw: "k8s:///api", wantError: true},
		{name: "empty container", raw: "k8s://prod/api/", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, err := ParseTarget(tt.raw)
			if tt.wantError {
				if err == nil {
					t.Fatalf("ParseTarget(%q) succeeded, want error", tt.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTarget(%q): %v", tt.raw, err)
			}
			if target.Runtime != "kubernetes" {
				t.Fatalf("Runtime = %q, want kubernetes", target.Runtime)
			}
			if target.Context != tt.wantCtx || target.Namespace != tt.wantNS || target.Name != tt.wantName || target.Container != tt.wantCtr {
				t.Fatalf("target = %#v, want context=%q namespace=%q name=%q container=%q", target, tt.wantCtx, tt.wantNS, tt.wantName, tt.wantCtr)
			}
		})
	}
}

func TestFindRunningDebuxContainerForTargetHonorsProfile(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			EphemeralContainers: []corev1.EphemeralContainer{
				{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name: "debux-general",
						Env:  []corev1.EnvVar{{Name: "DEBUX_SECURITY_PROFILE", Value: ProfileGeneral}},
					},
					TargetContainerName: "app",
				},
				{
					EphemeralContainerCommon: corev1.EphemeralContainerCommon{
						Name: "debux-restricted",
						Env:  []corev1.EnvVar{{Name: "DEBUX_SECURITY_PROFILE", Value: ProfileRestricted}},
					},
					TargetContainerName: "app",
				},
			},
		},
		Status: corev1.PodStatus{
			EphemeralContainerStatuses: []corev1.ContainerStatus{
				{Name: "debux-general", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "debux-restricted", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}

	if got := findRunningDebuxContainerForTarget(pod, "app", ProfileRestricted); got != "debux-restricted" {
		t.Fatalf("restricted container = %q", got)
	}
	if got := findRunningDebuxContainerForTarget(pod, "app", ProfileGeneral); got != "debux-general" {
		t.Fatalf("general container = %q", got)
	}
}

func TestKubernetesDebugTargetLabelIncludesContext(t *testing.T) {
	got := kubernetesDebugTargetLabel("prod-context", "gim", "api", "app")
	want := "prod-context:gim/api/app"
	if got != want {
		t.Fatalf("kubernetesDebugTargetLabel() = %q, want %q", got, want)
	}
}

func TestDebuxExecCommandQuotesOneShotCommand(t *testing.T) {
	cmd := debuxExecCommand([]string{"sh", "-c", "echo 'hello world'"})
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected command wrapper: %#v", cmd)
	}
	if !strings.Contains(cmd[2], "DEBUX_BANNER_SHOWN=1") {
		t.Fatalf("one-shot command should suppress the interactive banner: %q", cmd[2])
	}
	if !strings.Contains(cmd[2], "hello world") || !strings.Contains(cmd[2], "\\''") {
		t.Fatalf("command was not shell-quoted safely: %q", cmd[2])
	}
}

func TestTargetMountsCanForceReadOnly(t *testing.T) {
	info := container.InspectResponse{Mounts: []container.MountPoint{
		{Type: mount.TypeVolume, Name: "data", Destination: "/data", RW: true},
		{Type: mount.TypeBind, Source: "/host/config", Destination: "/config", RW: false},
		{Type: mount.TypeVolume, Name: "nix", Destination: "/nix/store", RW: true},
	}}

	mounts := targetMounts(info, false)
	if len(mounts) != 2 {
		t.Fatalf("targetMounts returned %d mount(s), want 2", len(mounts))
	}
	if mounts[0].Target != "/data" || mounts[0].ReadOnly {
		t.Fatalf("writable mount was not preserved: %#v", mounts[0])
	}
	if mounts[1].Target != "/config" || !mounts[1].ReadOnly {
		t.Fatalf("read-only mount was not preserved: %#v", mounts[1])
	}

	readOnlyMounts := targetMounts(info, true)
	for _, m := range readOnlyMounts {
		if !m.ReadOnly {
			t.Fatalf("mount %s was not forced read-only: %#v", m.Target, m)
		}
	}
}

func TestTargetKubernetesVolumeMountsCanForceReadOnly(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "app",
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "cache", MountPath: "/cache", ReadOnly: true},
			{Name: "sub", MountPath: "/sub", SubPath: "item"},
			{Name: "nix", MountPath: "/nix/store"},
		},
	}}}}

	mounts := targetKubernetesVolumeMounts(pod, "app", true, false)
	if len(mounts) != 2 {
		t.Fatalf("ephemeral volume mounts = %#v, want only /data and /cache", mounts)
	}
	for _, vm := range mounts {
		if !vm.ReadOnly {
			t.Fatalf("mount %s was not forced read-only: %#v", vm.MountPath, vm)
		}
		if vm.MountPath == "/sub" || vm.MountPath == "/nix/store" {
			t.Fatalf("reserved/subPath mount should have been skipped: %#v", vm)
		}
	}

	copyMounts := targetKubernetesVolumeMounts(pod, "app", false, true)
	if len(copyMounts) != 3 {
		t.Fatalf("copy volume mounts = %#v, want /data, /cache, and /sub", copyMounts)
	}
}

func TestIsDebuxDockerSidecarRequiresLabelOrDebuxImage(t *testing.T) {
	managed := container.Summary{
		ID:    "123456789abcdef",
		Names: []string{"/not-prefixed"},
		Labels: map[string]string{
			dockerLabelManagedBy: dockerLabelManagedByVal,
			dockerLabelKind:      dockerLabelKindSidecar,
		},
	}
	if !isDebuxDockerSidecar(managed) {
		t.Fatalf("label-managed sidecar was not recognized")
	}

	unrelated := container.Summary{ID: "abcdef123456", Names: []string{"/debux-important-db"}, Image: "postgres:latest"}
	if isDebuxDockerSidecar(unrelated) {
		t.Fatalf("unrelated debux-* container should not be treated as a debux sidecar")
	}

	legacy := container.Summary{ID: "abcdef123456", Names: []string{"/debux-api"}, Image: "ghcr.io/clement-tourriere/debux:latest"}
	if !isDebuxDockerSidecar(legacy) {
		t.Fatalf("legacy debux sidecar should be recognized")
	}
}
