package dockerclient

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func isolatedConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DOCKER_CONFIG", dir)
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "PODMAN_HOST"} {
		t.Setenv(key, "")
	}
	return dir
}

func writeContext(t *testing.T, dir, name, host string) {
	t.Helper()
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "meta.json"), []byte(`{"Endpoints":{"docker":{"Host":"`+host+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNewUsesSelectedContextAndExplicitHost(t *testing.T) {
	dir := isolatedConfig(t)
	writeContext(t, dir, "local-test", "unix:///tmp/debux-test.sock")
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext":"local-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cli, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if cli.DaemonHost() != "unix:///tmp/debux-test.sock" {
		t.Fatal(cli.DaemonHost())
	}
	_ = cli.Close()
	t.Setenv("DOCKER_CONTEXT", "missing")
	if _, err := New(); err == nil {
		t.Fatal("missing context must not choose default daemon")
	}
	t.Setenv("DOCKER_HOST", "unix:///tmp/explicit.sock")
	cli, err = New()
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if cli.DaemonHost() != "unix:///tmp/explicit.sock" {
		t.Fatal(cli.DaemonHost())
	}
}

func TestMalformedDockerConfigFailsClosed(t *testing.T) {
	dir := isolatedConfig(t)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{bad`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(); err == nil {
		t.Fatal("malformed config silently selected default daemon")
	}
}

func TestSSHContextExplainsUnsupportedTransport(t *testing.T) {
	dir := isolatedConfig(t)
	writeContext(t, dir, "remote", "ssh://remote")
	t.Setenv("DOCKER_CONTEXT", "remote")
	if _, err := New(); err == nil {
		t.Fatal("ssh context unexpectedly accepted")
	}
}

func TestPodmanExplicitHost(t *testing.T) {
	isolatedConfig(t)
	t.Setenv("PODMAN_HOST", "unix:///tmp/podman-test.sock")
	cli, err := NewForPodman()
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if cli.DaemonHost() != "unix:///tmp/podman-test.sock" {
		t.Fatal(cli.DaemonHost())
	}
}
