package runtime

import "fmt"

// KubernetesPermission is shared by doctor, generated RBAC documentation, and
// tests. Profiles change Linux privileges, not Kubernetes API permissions.
type KubernetesPermission struct {
	Verb, Resource, Subresource string
	ClusterScoped               bool
}

var KubernetesOperationModes = []string{"ephemeral", "copy", "pod", "node", "forward"}

func KubernetesPermissions(mode string) ([]KubernetesPermission, error) {
	if mode == "" {
		mode = "ephemeral"
	}
	var permissions []KubernetesPermission
	add := func(resource, subresource string, verbs ...string) {
		for _, verb := range verbs {
			permissions = append(permissions, KubernetesPermission{verb, resource, subresource, resource == "nodes"})
		}
	}
	switch mode {
	case "ephemeral":
		add("pods", "", "get", "list", "watch")
		add("pods", "ephemeralcontainers", "update")
		add("pods", "exec", "create", "get")
	case "copy":
		add("pods", "", "get", "list", "watch", "create", "delete")
		add("pods", "exec", "create", "get")
	case "pod", "node":
		add("pods", "", "get", "watch", "create", "delete")
		add("pods", "attach", "create", "get")
		if mode == "node" {
			add("nodes", "", "get", "list")
		}
	case "forward":
		add("pods", "", "get")
		add("pods", "portforward", "create")
	default:
		return nil, fmt.Errorf("unknown Kubernetes operation %q (expected ephemeral, copy, pod, node, or forward)", mode)
	}
	return permissions, nil
}
