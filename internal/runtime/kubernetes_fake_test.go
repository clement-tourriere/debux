package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// The fake clientset exercises the watch/retry/reuse logic that previously
// only ran against a live cluster (or not at all).

func testPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "app:1"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "app",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func TestUpdateEphemeralContainersRetriesOnConflict(t *testing.T) {
	pod := testPod("api")
	clientset := fake.NewClientset(pod)

	updates := 0
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "ephemeralcontainers" {
			return false, nil, nil
		}
		updates++
		if updates == 1 {
			// A controller touched the pod between our Get and the update.
			return true, nil, k8serrors.NewConflict(
				schema.GroupResource{Resource: "pods"}, "api", nil)
		}
		return false, nil, nil
	})

	ec := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debux-1", Image: "dbg:1"},
	}
	patched, err := updateEphemeralContainersWithRetry(t.Context(), clientset, "ns", "api", pod.DeepCopy(), ec)
	if err != nil {
		t.Fatalf("expected retry to succeed after one conflict, got %v", err)
	}
	if updates != 2 {
		t.Fatalf("update attempts = %d, want 2 (conflict then success)", updates)
	}
	found := false
	for _, got := range patched.Spec.EphemeralContainers {
		if got.Name == "debux-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("patched pod is missing the ephemeral container: %+v", patched.Spec.EphemeralContainers)
	}
}

func TestUpdateEphemeralContainersForbiddenSuggestsCopyMode(t *testing.T) {
	pod := testPod("api")
	clientset := fake.NewClientset(pod)
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, k8serrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "api", nil)
	})

	ec := corev1.EphemeralContainer{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "debux-1"},
	}
	_, err := updateEphemeralContainersWithRetry(t.Context(), clientset, "ns", "api", pod.DeepCopy(), ec)
	if err == nil || !strings.Contains(err.Error(), "--copy") {
		t.Fatalf("forbidden error should hint at --copy, got %v", err)
	}
}

// startPodWatchFeed wires a fake watcher per Watch call and returns a channel
// of the created watchers so tests can drive (or close) each one.
func startPodWatchFeed(clientset *fake.Clientset) <-chan *watch.FakeWatcher {
	watchers := make(chan *watch.FakeWatcher, 16)
	clientset.PrependWatchReactor("pods", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w := watch.NewFake()
		watchers <- w
		return true, w, nil
	})
	return watchers
}

func receiveTestValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for asynchronous test value")
		var zero T
		return zero
	}
}

func podWithEphemeralStatus(name, ecName string, state corev1.ContainerState) *corev1.Pod {
	pod := testPod(name)
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{Name: ecName, State: state}}
	return pod
}

func TestWaitForEphemeralContainerRunning(t *testing.T) {
	pending := podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})
	clientset := fake.NewClientset(pending)
	watchers := startPodWatchFeed(clientset)

	done := make(chan error, 1)
	go func() {
		done <- waitForEphemeralContainer(t.Context(), clientset, "ns", "api", "debux-1")
	}()

	w := receiveTestValue(t, watchers)
	w.Modify(podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	}))
	w.Modify(podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	}))

	if err := receiveTestValue(t, done); err != nil {
		t.Fatalf("expected success once the container runs, got %v", err)
	}
}

func TestWaitForEphemeralContainerImagePullFailureHasHint(t *testing.T) {
	pending := podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})
	clientset := fake.NewClientset(pending)
	watchers := startPodWatchFeed(clientset)

	done := make(chan error, 1)
	go func() {
		done <- waitForEphemeralContainer(t.Context(), clientset, "ns", "api", "debux-1")
	}()

	w := receiveTestValue(t, watchers)
	w.Modify(podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull access denied"},
	}))

	err := receiveTestValue(t, done)
	if err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") || !strings.Contains(err.Error(), "Hint:") {
		t.Fatalf("expected an image-pull failure with hint, got %v", err)
	}
}

func TestWaitForEphemeralContainerSurvivesWatchClose(t *testing.T) {
	// An apiserver restart or LB idle timeout closes the watch mid-wait; the
	// loop must re-watch from current state instead of aborting the session.
	pending := podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})
	clientset := fake.NewClientset(pending)
	watchers := startPodWatchFeed(clientset)

	done := make(chan error, 1)
	go func() {
		done <- waitForEphemeralContainer(t.Context(), clientset, "ns", "api", "debux-1")
	}()

	first := receiveTestValue(t, watchers)
	first.Stop()

	second := receiveTestValue(t, watchers)
	second.Modify(podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	}))

	if err := receiveTestValue(t, done); err != nil {
		t.Fatalf("expected the wait to survive one watch close, got %v", err)
	}
}

func TestWaitForEphemeralContainerObservesTransitionBetweenWatches(t *testing.T) {
	pending := podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
	})
	clientset := fake.NewClientset(pending)
	watchers := startPodWatchFeed(clientset)

	done := make(chan error, 1)
	go func() {
		done <- waitForEphemeralContainer(t.Context(), clientset, "ns", "api", "debux-1")
	}()

	first := receiveTestValue(t, watchers)
	running := podWithEphemeralStatus("api", "debux-1", corev1.ContainerState{
		Running: &corev1.ContainerStateRunning{},
	})
	if _, err := clientset.CoreV1().Pods("ns").Update(t.Context(), running, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	first.Stop()

	// The replacement watch is opened before the current snapshot is
	// evaluated, but no event is sent on it: success must come from the GET.
	receiveTestValue(t, watchers)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected the between-watch running state to be observed, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait missed the transition between watches")
	}
}

func TestWaitForEphemeralContainerGivesUpAfterRepeatedWatchCloses(t *testing.T) {
	clientset := fake.NewClientset(testPod("api"))
	watchers := startPodWatchFeed(clientset)

	done := make(chan error, 1)
	go func() {
		done <- waitForEphemeralContainer(t.Context(), clientset, "ns", "api", "debux-1")
	}()

	for range maxPodWatchRestarts + 1 {
		w := receiveTestValue(t, watchers)
		w.Stop()
	}

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "watch closed repeatedly") {
			t.Fatalf("expected repeated-close failure, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("wait did not give up after repeated watch closes")
	}
}

func TestWaitForPodRunningReportsFailedPhase(t *testing.T) {
	pending := testPod("dbg")
	pending.Status.Phase = corev1.PodPending
	clientset := fake.NewClientset(pending)
	watchers := startPodWatchFeed(clientset)
	failed := pending.DeepCopy()
	failed.Status.Phase = corev1.PodFailed

	done := make(chan error, 1)
	go func() {
		done <- waitForPodRunning(t.Context(), clientset, "ns", "dbg")
	}()

	w := receiveTestValue(t, watchers)
	w.Modify(failed)

	err := receiveTestValue(t, done)
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("expected failed-phase error, got %v", err)
	}
}

func debuxEphemeralPod(profile, user, image string) *corev1.Pod {
	pod := testPod("api")
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  "debux-42",
			Image: image,
			Env: []corev1.EnvVar{
				{Name: "DEBUX_DAEMON", Value: "1"},
				{Name: "DEBUX_SECURITY_PROFILE", Value: profile},
				{Name: "DEBUX_DEBUG_USER", Value: user},
			},
		},
		TargetContainerName: "app",
	}}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
		Name:  "debux-42",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}
	return pod
}

func TestEnsureKubernetesDebugContainerReusesMatchingSession(t *testing.T) {
	pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
	clientset := fake.NewClientset(pod)
	updates := 0
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		updates++
		return false, nil, nil
	})

	name, _, err := ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, clientset, "ns", pod,
		"app", "ctx", DebugOpts{Image: "dbg:1", Profile: ProfileGeneral})
	if err != nil {
		t.Fatal(err)
	}
	if name != "debux-42" {
		t.Fatalf("reused container = %q, want debux-42", name)
	}
	if updates != 0 {
		t.Fatalf("reuse must not patch the pod, saw %d update(s)", updates)
	}
}

func TestEnsureKubernetesDebugContainerRefusesProfileMismatch(t *testing.T) {
	// A running session with a *more privileged* profile must not satisfy a
	// request for a restricted one: reuse would silently ignore the flag.
	pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
	clientset := fake.NewClientset(pod)
	watchers := startPodWatchFeed(clientset)

	done := make(chan struct{})
	var name string
	var err error
	go func() {
		defer close(done)
		name, _, err = ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, clientset, "ns", pod,
			"app", "ctx", DebugOpts{Image: "dbg:1", Profile: ProfileRestricted})
	}()

	// A new ephemeral container is created and waited on; report it running.
	w := receiveTestValue(t, watchers)
	patched, getErr := clientset.CoreV1().Pods("ns").Get(t.Context(), "api", metav1.GetOptions{})
	if getErr != nil {
		t.Fatal(getErr)
	}
	created := ""
	for _, ec := range patched.Spec.EphemeralContainers {
		if ec.Name != "debux-42" {
			created = ec.Name
		}
	}
	if created == "" {
		t.Fatal("expected a new ephemeral container to be created for the profile mismatch")
	}
	patched.Status.EphemeralContainerStatuses = append(patched.Status.EphemeralContainerStatuses,
		corev1.ContainerStatus{Name: created, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	w.Modify(patched)

	receiveTestValue(t, done)
	if err != nil {
		t.Fatal(err)
	}
	if name == "debux-42" {
		t.Fatal("a general-profile session must not be reused for --profile=restricted")
	}
}

func TestEnsureKubernetesDebugContainerFreshReplacesBeforeKillingAll(t *testing.T) {
	pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
	second := pod.Spec.EphemeralContainers[0]
	second.Name = "debux-43"
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, second)
	pod.Status.EphemeralContainerStatuses = append(pod.Status.EphemeralContainerStatuses, corev1.ContainerStatus{
		Name:  second.Name,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})

	clientset := fake.NewClientset(pod)
	watchers := startPodWatchFeed(clientset)
	killed := make(chan string, 2)
	killer := func(_ context.Context, _ *rest.Config, _ kubernetes.Interface, _, _, containerName string) error {
		killed <- containerName
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, _, err := ensureKubernetesDebugContainerWithKiller(t.Context(), &rest.Config{}, clientset, "ns", pod,
			"app", "ctx", DebugOpts{Image: "dbg:1", Profile: ProfileGeneral, Fresh: true}, killer)
		done <- err
	}()

	w := receiveTestValue(t, watchers)
	select {
	case name := <-killed:
		t.Fatalf("superseded container %q was killed before its replacement started", name)
	default:
	}

	patched, err := clientset.CoreV1().Pods("ns").Get(t.Context(), "api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	created := patched.Spec.EphemeralContainers[len(patched.Spec.EphemeralContainers)-1].Name
	patched.Status.EphemeralContainerStatuses = append(patched.Status.EphemeralContainerStatuses, corev1.ContainerStatus{
		Name:  created,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	})
	w.Modify(patched)

	if err := receiveTestValue(t, done); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{
		receiveTestValue(t, killed): true,
		receiveTestValue(t, killed): true,
	}
	for _, want := range []string{"debux-42", "debux-43"} {
		if !got[want] {
			t.Fatalf("superseded containers killed = %v, missing %s", got, want)
		}
	}
}

func TestEnsureKubernetesDebugContainerFreshFailureKeepsOldSession(t *testing.T) {
	pod := debuxEphemeralPod(ProfileGeneral, "", "dbg:1")
	clientset := fake.NewClientset(pod)
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		return true, testPod("api"), nil // admission stripped the replacement
	})
	kills := 0
	killer := func(_ context.Context, _ *rest.Config, _ kubernetes.Interface, _, _, _ string) error {
		kills++
		return nil
	}

	_, _, err := ensureKubernetesDebugContainerWithKiller(t.Context(), &rest.Config{}, clientset, "ns", pod,
		"app", "ctx", DebugOpts{Image: "dbg:1", Profile: ProfileGeneral, Fresh: true}, killer)
	if err == nil {
		t.Fatal("expected replacement creation to fail")
	}
	if kills != 0 {
		t.Fatalf("failed replacement killed %d working session(s)", kills)
	}
}

func TestEnsureKubernetesDebugContainerRejectsTerminatingPod(t *testing.T) {
	pod := testPod("api")
	now := metav1.Now()
	pod.DeletionTimestamp = &now
	clientset := fake.NewClientset(pod)

	_, _, err := ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, clientset, "ns", pod,
		"app", "ctx", DebugOpts{Image: "dbg:1"})
	if err == nil || !strings.Contains(err.Error(), "terminating") {
		t.Fatalf("expected terminating-pod rejection, got %v", err)
	}
}

func TestEnsureKubernetesDebugContainerReportsStrippedContainer(t *testing.T) {
	// Admission webhooks can silently strip the ephemeral container from the
	// update response; that must be reported, not waited on forever.
	pod := testPod("api")
	clientset := fake.NewClientset(pod)
	clientset.PrependReactor("update", "pods", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "ephemeralcontainers" {
			return false, nil, nil
		}
		return true, testPod("api"), nil // response without the ephemeral container
	})

	_, _, err := ensureKubernetesDebugContainer(t.Context(), &rest.Config{}, clientset, "ns", pod,
		"app", "ctx", DebugOpts{Image: "dbg:1"})
	if err == nil || !strings.Contains(err.Error(), "webhook") {
		t.Fatalf("expected admission-strip diagnosis, got %v", err)
	}
}

func copyPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "ns",
			Labels: map[string]string{
				debuxManagedByLabelKey: debuxManagedByLabelValue,
				debuxModeLabelKey:      debuxModeCopy,
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestDeleteAllKubernetesCopyPodsSweepsKeptAndExpired(t *testing.T) {
	clientset := fake.NewClientset(
		copyPod("debux-copy-live", corev1.PodRunning),
		copyPod("debux-copy-expired", corev1.PodFailed), // activeDeadlineSeconds hit
		testPod("unrelated"),
	)

	deleted, err := deleteAllKubernetesCopyPods(t.Context(), clientset, "ns")
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (running and expired copy pods)", deleted)
	}
	if _, err := clientset.CoreV1().Pods("ns").Get(t.Context(), "unrelated", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated pod must survive the sweep: %v", err)
	}
}
