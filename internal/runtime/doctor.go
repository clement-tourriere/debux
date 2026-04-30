package runtime

import (
	"context"
	"fmt"
	"strings"

	containertypes "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	CheckPass = "pass"
	CheckWarn = "warn"
	CheckFail = "fail"
)

// DoctorCheck is a single diagnostic result.
type DoctorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

func pass(name, detail string) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckPass, Detail: detail}
}
func warn(name, detail string) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckWarn, Detail: detail}
}
func fail(name, detail string) DoctorCheck {
	return DoctorCheck{Name: name, Status: CheckFail, Detail: detail}
}

// DockerDoctor checks local Docker connectivity and debux-managed sessions.
func DockerDoctor(ctx context.Context, targetName ...string) []DoctorCheck {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return []DoctorCheck{fail("Docker client", fmt.Sprintf("connecting to Docker: %v", err))}
	}
	defer func() { _ = cli.Close() }()

	checks := []DoctorCheck{}
	ping, err := cli.Ping(ctx)
	if err != nil {
		return append(checks, fail("Docker daemon", fmt.Sprintf("daemon is not reachable: %v", err)))
	}
	checks = append(checks, pass("Docker daemon", fmt.Sprintf("reachable (API %s)", ping.APIVersion)))

	if len(targetName) > 0 && strings.TrimSpace(targetName[0]) != "" {
		info, err := cli.ContainerInspect(ctx, targetName[0])
		if err != nil {
			checks = append(checks, fail("Target container", fmt.Sprintf("inspecting %q: %v", targetName[0], err)))
		} else if info.State != nil && info.State.Running {
			checks = append(checks, pass("Target container", fmt.Sprintf("%s is running", targetName[0])))
		} else {
			checks = append(checks, fail("Target container", fmt.Sprintf("%s is not running", targetName[0])))
		}
	}

	containers, err := cli.ContainerList(ctx, containertypes.ListOptions{})
	if err != nil {
		checks = append(checks, warn("Docker containers", fmt.Sprintf("could not list containers: %v", err)))
		return checks
	}

	running := 0
	debug := 0
	for _, c := range containers {
		if c.State == "running" {
			running++
		}
		if isDebuxDockerSidecar(c) && c.State == "running" {
			debug++
		}
	}
	checks = append(checks, pass("Docker containers", fmt.Sprintf("%d running container(s), %d active debux session(s)", running, debug)))
	return checks
}

// KubernetesDoctor checks Kubernetes connectivity, target existence, and common RBAC permissions.
func KubernetesDoctor(ctx context.Context, kubeconfig, kubeContext, namespace, podName, containerName, profile string) []DoctorCheck {
	_, clientset, err := getK8sClient(kubeconfig, kubeContext)
	if err != nil {
		return []DoctorCheck{fail("Kubernetes client", err.Error())}
	}

	resolvedNamespace := resolveTargetNamespace(namespace, kubeconfig, kubeContext)
	checks := []DoctorCheck{
		pass("Kubernetes client", "configuration loaded"),
		pass("Kubernetes namespace", resolvedNamespace),
	}
	if kubeContext != "" {
		checks = append(checks, pass("Kubernetes context", kubeContext))
	}

	if podName != "" {
		pod, err := clientset.CoreV1().Pods(resolvedNamespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			checks = append(checks, fail("Target pod", fmt.Sprintf("getting %s/%s: %v", resolvedNamespace, podName, err)))
		} else {
			checks = append(checks, pass("Target pod", fmt.Sprintf("%s/%s is %s", resolvedNamespace, podName, pod.Status.Phase)))
			if container, err := selectKubernetesTargetContainer(pod, containerName); err == nil {
				checks = append(checks, pass("Target container", container))
			} else {
				checks = append(checks, warn("Target container", err.Error()))
			}
		}
	}

	checks = append(checks,
		kubernetesAccessCheck(ctx, clientset, resolvedNamespace, "list", "pods", ""),
		kubernetesAccessCheck(ctx, clientset, resolvedNamespace, "get", "pods", ""),
		kubernetesAccessCheck(ctx, clientset, resolvedNamespace, "update", "pods", "ephemeralcontainers"),
		kubernetesAccessCheck(ctx, clientset, resolvedNamespace, "create", "pods", "exec"),
		kubernetesAccessCheck(ctx, clientset, resolvedNamespace, "create", "pods", ""),
	)

	switch profile {
	case ProfileGeneral, "":
		checks = append(checks, warn("Security profile", "general runs a root debug container with debugging capabilities inside the pod"))
	case ProfileRestricted:
		checks = append(checks, pass("Security profile", "restricted: non-root, drops capabilities, runtime default seccomp"))
	case ProfileBaseline:
		checks = append(checks, warn("Security profile", "baseline leaves securityContext unset; cluster policy decides defaults"))
	case ProfileNetadmin:
		checks = append(checks, warn("Security profile", "netadmin adds network capabilities"))
	case ProfileSysadmin:
		checks = append(checks, warn("Security profile", "sysadmin is privileged; use as break-glass only"))
	default:
		checks = append(checks, fail("Security profile", fmt.Sprintf("unknown profile %q", profile)))
	}

	return checks
}

func kubernetesAccessCheck(ctx context.Context, clientset *kubernetes.Clientset, namespace, verb, resource, subresource string) DoctorCheck {
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        verb,
				Group:       "",
				Resource:    resource,
				Subresource: subresource,
			},
		},
	}

	created, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	name := fmt.Sprintf("RBAC %s %s", verb, resource)
	if subresource != "" {
		name += "/" + subresource
	}
	if err != nil {
		return warn(name, fmt.Sprintf("could not check: %v", err))
	}
	if created.Status.Allowed {
		return pass(name, "allowed")
	}
	if created.Status.Reason != "" {
		return fail(name, created.Status.Reason)
	}
	return fail(name, "denied")
}
