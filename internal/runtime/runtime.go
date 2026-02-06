package runtime

import (
	"fmt"
	"strings"
)

const DefaultImage = "ghcr.io/ctourriere/debux:latest"

// Target represents a parsed container/pod target.
type Target struct {
	Runtime   string // "docker", "containerd", "kubernetes"
	Name      string // container name/id or pod name
	Namespace string // k8s namespace (default: "default")
	Container string // k8s container within pod (optional)
}

// DebugOpts are options for debugging a running container.
type DebugOpts struct {
	Image        string
	Privileged   bool
	User         string
	AutoRemove   bool
	Kubeconfig   string
	ShareVolumes bool // share target container's volumes (default: true)
}

// PodOpts are options for creating a standalone debug pod.
type PodOpts struct {
	Image       string
	Namespace   string
	Kubeconfig  string
	Keep        bool
	HostNetwork bool
	Privileged  bool
	User        string
}

// ImageOpts are options for debugging a Docker image directly.
type ImageOpts struct {
	DebugImage string
	Privileged bool
	User       string
	AutoRemove bool
}

// ParseTarget parses a target string into a Target struct.
//
// Formats:
//
//	<name>                          → docker (default)
//	docker://<name>                 → docker
//	containerd://<name>             → containerd
//	nerdctl://<name>                → containerd
//	k8s://<pod>                     → kubernetes (default namespace)
//	k8s://<namespace>/<pod>         → kubernetes
//	k8s://<namespace>/<pod>/<ctr>   → kubernetes (specific container)
func ParseTarget(raw string) (*Target, error) {
	if raw == "" {
		return nil, fmt.Errorf("empty target")
	}

	// Check for schema prefix
	if idx := strings.Index(raw, "://"); idx != -1 {
		schema := raw[:idx]
		rest := raw[idx+3:]

		switch schema {
		case "docker":
			return &Target{Runtime: "docker", Name: rest}, nil

		case "containerd", "nerdctl":
			return &Target{Runtime: "containerd", Name: rest}, nil

		case "k8s":
			return parseK8sTarget(rest)

		default:
			return nil, fmt.Errorf("unknown schema: %s", schema)
		}
	}

	// No schema — default to Docker
	return &Target{Runtime: "docker", Name: raw}, nil
}

func parseK8sTarget(rest string) (*Target, error) {
	t := &Target{Runtime: "kubernetes", Namespace: "default"}

	// Empty rest means k8s:// — list all pods
	if rest == "" {
		return t, nil
	}

	parts := strings.Split(rest, "/")

	switch len(parts) {
	case 1:
		// k8s://<pod>
		t.Name = parts[0]
	case 2:
		// k8s://<namespace>/<pod> or k8s://<namespace>/
		t.Namespace = parts[0]
		t.Name = parts[1]
	case 3:
		// k8s://<namespace>/<pod>/<container>
		t.Namespace = parts[0]
		t.Name = parts[1]
		t.Container = parts[2]
	default:
		return nil, fmt.Errorf("invalid k8s target format: %s", rest)
	}

	return t, nil
}
