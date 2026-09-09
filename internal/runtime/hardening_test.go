package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
)

func TestSessionOptionsCompatibility(t *testing.T) {
	base := DebugOpts{Image: "debug:1", Profile: ProfileGeneral, ShareVolumes: true}
	mutations := map[string]func(*DebugOpts){
		"image": func(o *DebugOpts) { o.Image = "debug:2" }, "profile": func(o *DebugOpts) { o.Profile = ProfileRestricted },
		"user": func(o *DebugOpts) { o.User = "65534" }, "privileged": func(o *DebugOpts) { o.Privileged = true },
		"volumes": func(o *DebugOpts) { o.ShareVolumes = false }, "read-only": func(o *DebugOpts) { o.ReadOnlyVolumes = true },
		"env": func(o *DebugOpts) { o.Env = []string{"TOKEN=secret"} }, "tools": func(o *DebugOpts) { o.Tools = []string{"gdb"} },
		"caps": func(o *DebugOpts) { o.CapAdd = []string{"NET_ADMIN"} }, "pull": func(o *DebugOpts) { o.PullPolicy = "Always" },
	}
	hash := sessionOptionsHash(base, "target-generation")
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			if sessionOptionsHash(o, "target-generation") == hash {
				t.Fatal("creation options must change identity")
			}
		})
	}
	if sessionOptionsHash(base, "new-target-generation") == hash {
		t.Fatal("target restart must invalidate reuse")
	}
	base.Command = []string{"new-command"}
	base.Fresh = true
	if sessionOptionsHash(base, "target-generation") != hash {
		t.Fatal("commands and fresh are not creation settings")
	}
	a, b := base, base
	a.CapAdd = []string{"net_admin", "CAP_SYS_PTRACE"}
	b.CapAdd = []string{"SYS_PTRACE", "NET_ADMIN", "NET_ADMIN"}
	if sessionOptionsHash(a) != sessionOptionsHash(b) {
		t.Fatal("equivalent capabilities should normalize")
	}
}

func TestKubernetesReuseRequiresMatchingCreationOptions(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
			opts := DebugOpts{Image: "dbg:1", Profile: ProfileGeneral}
			if changed {
				opts.ReadOnlyVolumes = true
				opts.ShareVolumes = true
				opts.Env = []string{"PROBE=new"}
				opts.Tools = []string{"gdb"}
				opts.CapAdd = []string{"NET_ADMIN"}
			}
			cs := fake.NewClientset(pod)
			stop := errors.New("stop before contacting a kubelet")
			cs.PrependReactor("update", "pods", func(a ktesting.Action) (bool, k8sruntime.Object, error) {
				updated := a.(ktesting.UpdateAction).GetObject().(*corev1.Pod)
				created := updated.Spec.EphemeralContainers[len(updated.Spec.EphemeralContainers)-1]
				found := false
				for _, e := range created.Env {
					if e.Name == sessionOptionsEnv && e.Value == sessionOptionsHash(opts, "") {
						found = true
					}
				}
				if !found {
					t.Error("new container is missing compatibility metadata")
				}
				return true, nil, stop
			})
			name, _, err := ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, cs, "ns", pod, "app", "ctx", opts)
			if changed {
				if err == nil || name == "debux-42" || len(cs.Actions()) == 0 {
					t.Fatalf("incompatible options silently reused: %s %v %v", name, err, cs.Actions())
				}
			} else if err != nil || name != "debux-42" || len(cs.Actions()) != 0 {
				t.Fatalf("compatible reuse: %s %v", name, err)
			}
		})
	}
}

func TestSharedPIDNamespaceFailsClosed(t *testing.T) {
	yes := true
	for _, pod := range []*corev1.Pod{{Spec: corev1.PodSpec{HostPID: true}}, {Spec: corev1.PodSpec{ShareProcessNamespace: &yes}}} {
		cs := fake.NewClientset(pod)
		_, _, err := ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, cs, "ns", pod, "app", "", DebugOpts{})
		if err == nil || len(cs.Actions()) != 0 {
			t.Fatalf("unsafe PID namespace reached API: %v", err)
		}
	}
	if err := validateKubernetesTargetNamespace(&corev1.Pod{}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedSessionIdentityCannotFallBack(t *testing.T) {
	pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
	pod.UID = types.UID("original")
	second := *pod.Spec.EphemeralContainers[0].DeepCopy()
	second.Name = "debux-43"
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, second)
	pod.Status.EphemeralContainerStatuses = append(pod.Status.EphemeralContainerStatuses, corev1.ContainerStatus{Name: second.Name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	sessions := kubernetesSessionsForPod(pod, "ctx")
	if len(sessions) != 2 {
		t.Fatal(sessions)
	}
	selected := sessions[1]
	if selected.DebugName != "debux-43" || selected.ID != "original" {
		t.Fatal(selected)
	}
	if err := validateKubernetesSession(pod, selected); err != nil {
		t.Fatal(err)
	}
	pod.Status.EphemeralContainerStatuses = pod.Status.EphemeralContainerStatuses[:1]
	if err := validateKubernetesSession(pod, selected); err == nil {
		t.Fatal("must not fall back to debux-42")
	}
	pod.UID = "replacement"
	if err := validateKubernetesSession(pod, sessions[0]); err == nil {
		t.Fatal("must not use a recycled pod name")
	}
}

func TestDockerSessionRejectsRecycledNamesAndOtherManagedKinds(t *testing.T) {
	info := container.InspectResponse{ID: "id", Name: "/debux-app", State: &container.State{Running: true}, Config: &container.Config{Labels: map[string]string{dockerLabelManagedBy: dockerLabelManagedByVal, dockerLabelKind: dockerLabelKindSidecar}}}
	selected := DebugSessionInfo{ID: "id", DebugName: "debux-app"}
	if err := validateDockerSession(info, selected); err != nil {
		t.Fatal(err)
	}
	selected.ID = "recycled"
	if err := validateDockerSession(info, selected); err == nil {
		t.Fatal("recycled identity accepted")
	}
	info.Config.Labels[dockerLabelKind] = dockerLabelKindImageTarget
	selected.ID = "id"
	if err := validateDockerSession(info, selected); err == nil {
		t.Fatal("non-sidecar accepted by legacy-name fallback")
	}
}

func TestCopyDockerOutputRejectsTruncation(t *testing.T) {
	frame := append([]byte{1, 0, 0, 0, 0, 0, 0, 3}, []byte("out")...)
	frame = append(frame, 2, 0, 0, 0, 0, 0, 0, 3)
	frame = append(frame, []byte("err")...)
	var out, stderr bytes.Buffer
	if err := copyDockerOutput(&out, &stderr, bytes.NewReader(frame)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "out" || stderr.String() != "err" {
		t.Fatalf("streams mixed: %q %q", &out, &stderr)
	}
	for _, cut := range []int{1, 7, 9, len(frame) - 1} {
		if err := copyDockerOutput(io.Discard, io.Discard, bytes.NewReader(frame[:cut])); err == nil {
			t.Errorf("truncation at %d accepted", cut)
		}
	}
}

func TestSessionInputReturnsOwnershipBeforeNextSession(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	for range 8 {
		stop, err := startSessionInput(input, io.Discard, func() {}, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		stop()
		stop() // idempotent; no pending reader may consume the next key
	}
	if _, err := writer.Write([]byte("next-TUI-key")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	got, err := io.ReadAll(input)
	if err != nil || string(got) != "next-TUI-key" {
		t.Fatalf("stdin ownership lost: %q %v", got, err)
	}
}

func TestWaitDockerExecRequiresTerminalStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		running bool
		exit    int
		wantErr bool
	}{{"success", false, 0, false}, {"exit-42", false, 42, true}, {"disconnected", true, 0, true}} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"Running": tc.running, "ExitCode": tc.exit})
			}))
			defer srv.Close()
			cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.55"))
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
			defer cancel()
			err = waitDockerExecExit(ctx, cli, "id")
			if (err != nil) != tc.wantErr {
				t.Fatalf("terminal status: %v", err)
			}
			if tc.exit != 0 {
				var exit *ExitError
				if !errors.As(err, &exit) || exit.Code != tc.exit {
					t.Fatalf("exit code lost: %v", err)
				}
			}
		})
	}
}

func TestDockerExecPropagatesTruncatedOutput(t *testing.T) {
	var creates atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the attach request before closing the hijacked socket; unread
		// request bytes can otherwise produce a TCP reset (especially under -race).
		_, _ = io.Copy(io.Discard, r.Body)
		p := r.URL.Path
		switch {
		case strings.HasSuffix(p, "/exec"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": fmt.Sprint(creates.Add(1))})
		case strings.HasSuffix(p, "/start"):
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = rw.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
			if strings.Contains(p, "/2/") {
				_, _ = rw.Write([]byte{1, 0, 0, 0, 0, 0, 0, 10})
				_, _ = rw.WriteString("bad")
			}
			_ = rw.Flush()
			_ = conn.Close()
		case strings.HasSuffix(p, "/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Running":false,"ExitCode":0}`)
		default:
			t.Errorf("unexpected request: %s", p)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(srv.URL, "http://")), client.WithAPIVersion("1.55"))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer writer.Close()
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	err = execInContainer(t.Context(), cli, "id", []string{"true"})
	if err == nil || !strings.Contains(err.Error(), "reading exec output") {
		t.Fatalf("truncated output reported success: %v", err)
	}
	_, _ = writer.Write([]byte("next"))
	_ = writer.Close()
	got, _ := io.ReadAll(input)
	if string(got) != "next" {
		t.Fatalf("session consumed future stdin: %q", got)
	}
}

func TestCleanupSurvivesCancellationWithDeadlineAndUID(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	cleanupResource(ctx, "test", func(ctx context.Context) error {
		called = true
		if ctx.Err() != nil {
			t.Error("cleanup inherited cancellation")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded cleanup")
		}
		return nil
	})
	if !called {
		t.Fatal("cleanup skipped")
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "copy", Namespace: "ns", UID: "selected"}}
	cs := fake.NewClientset(pod)
	cleanupKubernetesPod(ctx, cs, pod)
	action := cs.Actions()[0].(ktesting.DeleteAction)
	if p := action.GetDeleteOptions().Preconditions; p == nil || p.UID == nil || *p.UID != "selected" {
		t.Fatal("cleanup may delete replacement pod")
	}
}

func TestRBACMatchesOperation(t *testing.T) {
	for _, mode := range KubernetesOperationModes {
		permissions, err := KubernetesPermissions(mode)
		if err != nil {
			t.Fatal(err)
		}
		has := func(verb, resource, sub string) bool {
			for _, p := range permissions {
				if p.Verb == verb && p.Resource == resource && p.Subresource == sub {
					return true
				}
			}
			return false
		}
		if mode != "forward" && !has("watch", "pods", "") {
			t.Errorf("%s missing pod watch", mode)
		}
		if (mode == "ephemeral" || mode == "forward") && has("create", "pods", "") {
			t.Errorf("%s needlessly requests pod creation", mode)
		}
		if mode == "ephemeral" && !has("update", "pods", "ephemeralcontainers") {
			t.Fatal("missing ephemeral RBAC")
		}
	}
}
