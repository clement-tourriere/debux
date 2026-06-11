// Package dockerclient creates Docker API clients that honor the same
// configuration the docker CLI uses: DOCKER_HOST first, then the selected
// docker context (DOCKER_CONTEXT or config.json's currentContext), then the
// default socket. Without this, colima, Docker Desktop secondary endpoints,
// and remote contexts silently point debux at the wrong daemon.
package dockerclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/moby/moby/client"
)

// New connects to the Docker daemon the docker CLI would talk to.
func New() (*client.Client, error) {
	opts := []client.Opt{client.FromEnv}
	if os.Getenv("DOCKER_HOST") == "" {
		host, tlsDir, err := currentContextEndpoint()
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(host, "ssh://") {
			return nil, fmt.Errorf("docker context endpoint %s uses ssh, which debux does not support; forward the remote socket and set DOCKER_HOST instead (e.g. ssh -nNT -L /tmp/docker.sock:/var/run/docker.sock <host>)", host)
		}
		if host != "" {
			opts = append(opts, client.WithHost(host))
		}
		if tlsDir != "" {
			ca := filepath.Join(tlsDir, "ca.pem")
			cert := filepath.Join(tlsDir, "cert.pem")
			key := filepath.Join(tlsDir, "key.pem")
			if fileExists(ca) && fileExists(cert) && fileExists(key) {
				opts = append(opts, client.WithTLSClientConfig(ca, cert, key))
			}
		}
	}
	return client.New(opts...)
}

// NewForPodman connects to a podman daemon through its Docker-compatible API.
// PODMAN_HOST and DOCKER_HOST take precedence; otherwise the standard podman
// socket locations are probed.
func NewForPodman() (*client.Client, error) {
	if host := os.Getenv("PODMAN_HOST"); host != "" {
		return client.New(client.FromEnv, client.WithHost(host))
	}
	if os.Getenv("DOCKER_HOST") != "" {
		return client.New(client.FromEnv)
	}
	for _, socket := range podmanSocketCandidates() {
		if st, err := os.Stat(socket); err == nil && st.Mode()&os.ModeSocket != 0 {
			return client.New(client.FromEnv, client.WithHost("unix://"+socket))
		}
	}
	return nil, fmt.Errorf("no podman socket found; enable it with `systemctl --user enable --now podman.socket` (rootless), `podman machine start` (macOS), or set DOCKER_HOST/PODMAN_HOST")
}

func podmanSocketCandidates() []string {
	var candidates []string
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "podman", "podman.sock"))
	}
	return append(candidates,
		"/run/podman/podman.sock",
		"/var/run/podman/podman.sock",
	)
}

func configDir() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker")
}

// currentContextEndpoint resolves the docker endpoint of the CLI's selected
// context. It returns empty values when the default context is selected or no
// docker CLI configuration exists.
func currentContextEndpoint() (host, tlsDir string, err error) {
	dir := configDir()
	if dir == "" {
		return "", "", nil
	}
	name := os.Getenv("DOCKER_CONTEXT")
	if name == "" {
		data, err := os.ReadFile(filepath.Join(dir, "config.json"))
		if err != nil {
			return "", "", nil // no docker CLI config: use the default socket
		}
		var cfg struct {
			CurrentContext string `json:"currentContext"`
		}
		if json.Unmarshal(data, &cfg) != nil {
			return "", "", nil
		}
		name = cfg.CurrentContext
	}
	if name == "" || name == "default" {
		return "", "", nil
	}

	digest := sha256.Sum256([]byte(name))
	id := hex.EncodeToString(digest[:])
	metaPath := filepath.Join(dir, "contexts", "meta", id, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", "", fmt.Errorf("docker context %q is selected but its metadata cannot be read (%v); run `docker context ls` or set DOCKER_HOST", name, err)
	}
	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", "", fmt.Errorf("docker context %q: parsing %s: %w", name, metaPath, err)
	}
	ep, ok := meta.Endpoints["docker"]
	if !ok || ep.Host == "" {
		return "", "", fmt.Errorf("docker context %q has no docker endpoint; run `docker context inspect %s` or set DOCKER_HOST", name, name)
	}

	tlsDir = filepath.Join(dir, "contexts", "tls", id, "docker")
	if st, statErr := os.Stat(tlsDir); statErr != nil || !st.IsDir() {
		tlsDir = ""
	}
	return ep.Host, tlsDir, nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
