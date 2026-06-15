package runtime

import "time"

const (
	DebugSessionKindDockerSidecar       = "docker-sidecar"
	DebugSessionKindKubernetesEphemeral = "k8s-ephemeral"
	DebugSessionKindKubernetesCopyPod   = "k8s-copy"
)

// DebugSessionInfo describes a currently attachable debux session.
// Target is the URI users can pass back to debux to reattach.
type DebugSessionInfo struct {
	Runtime         string
	Kind            string
	Target          string
	Name            string
	Context         string
	Namespace       string
	TargetContainer string
	DebugName       string
	Source          string
	Image           string
	User            string
	Profile         string
	Status          string
	StartedAt       time.Time
	ExpiresIn       time.Duration
	HasExpiry       bool
}
