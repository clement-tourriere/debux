package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/clement-tourriere/debux/internal/runtime"
	"github.com/spf13/cobra"
)

const (
	completionPodCacheVersion          = 1
	completionPodCacheFreshFor         = 2 * time.Minute
	completionPodCacheStaleFor         = 30 * time.Minute
	completionPodCacheRefreshTimeout   = 30 * time.Second
	completionPodCacheRefreshLockStale = 45 * time.Second
	completionPodCacheMaxResults       = 500
)

type completionPodCache struct {
	Version int               `json:"version"`
	SavedAt time.Time         `json:"savedAt"`
	Limited bool              `json:"limited"`
	Pods    []runtime.PodInfo `json:"pods"`
}

func newCompletionCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "__completion_cache",
		Hidden:        true,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	var kubeconfig string
	var kubeContext string
	var namespace string

	podsCmd := &cobra.Command{
		Use:   "k8s-pods",
		Short: "Refresh Kubernetes pod completion cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(context.Background(), completionPodCacheRefreshTimeout)
			defer cancel()

			if namespace == "" {
				namespace = runtime.KubernetesDefaultNamespace(kubeconfig, kubeContext)
			}
			defer releaseCompletionPodCacheRefreshLock(kubeconfig, kubeContext, namespace)

			pruneCompletionPodCaches()
			pods, limited, err := runtime.KubernetesBrowsePods(ctx, kubeconfig, kubeContext, namespace, "", completionPodCacheMaxResults)
			if err != nil {
				return err
			}
			return writeCompletionPodCache(kubeconfig, kubeContext, namespace, pods, limited)
		},
	}
	podsCmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "Kubeconfig path")
	podsCmd.Flags().StringVar(&kubeContext, "context", "", "Kube context")
	podsCmd.Flags().StringVar(&namespace, "namespace", "", "Kubernetes namespace")

	cmd.AddCommand(podsCmd)
	return cmd
}

func readCompletionPodCache(kubeconfig, kubeContext, namespace string) (completionPodCache, bool) {
	path, err := completionPodCachePath(kubeconfig, kubeContext, namespace)
	if err != nil {
		return completionPodCache{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return completionPodCache{}, false
	}
	var cache completionPodCache
	if err := json.Unmarshal(data, &cache); err != nil {
		_ = os.Remove(path)
		return completionPodCache{}, false
	}
	if cache.Version != completionPodCacheVersion || cache.SavedAt.IsZero() {
		_ = os.Remove(path)
		return completionPodCache{}, false
	}
	// A future SavedAt (clock step-back between write and read) would
	// otherwise count as fresh forever and never refresh.
	age := time.Since(cache.SavedAt)
	if age > completionPodCacheStaleFor || age < -time.Minute {
		_ = os.Remove(path)
		return completionPodCache{}, false
	}
	return cache, true
}

func writeCompletionPodCache(kubeconfig, kubeContext, namespace string, pods []runtime.PodInfo, limited bool) error {
	path, err := completionPodCachePath(kubeconfig, kubeContext, namespace)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	cache := completionPodCache{
		Version: completionPodCacheVersion,
		SavedAt: time.Now(),
		Limited: limited,
		Pods:    pods,
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return err
	}
	// Write atomically: shell completion runs many debux processes in
	// parallel, and an in-place truncate-and-write can leave torn JSON.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".debux-cache-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func startCompletionPodCacheRefresh(kubeconfig, kubeContext, namespace string) bool {
	lockPath, err := completionPodCacheRefreshLockPath(kubeconfig, kubeContext, namespace)
	if err != nil {
		return false
	}
	if !acquireCompletionRefreshLock(lockPath) {
		return false
	}

	exe, err := os.Executable()
	if err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	args := []string{"__completion_cache", "k8s-pods"}
	if kubeconfig != "" {
		args = append(args, "--kubeconfig", kubeconfig)
	}
	if kubeContext != "" {
		args = append(args, "--context", kubeContext)
	}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	if err := cmd.Process.Release(); err != nil {
		_ = os.Remove(lockPath)
		return false
	}
	return true
}

func acquireCompletionRefreshLock(lockPath string) bool {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return false
	}
	file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
		_ = file.Close()
		return true
	}
	if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > completionPodCacheRefreshLockStale {
		// Steal the stale lock via rename so exactly one contender wins;
		// a plain remove+recreate lets a second process delete the fresh
		// lock the first one just created.
		stalePath := fmt.Sprintf("%s.stale.%d", lockPath, os.Getpid())
		if os.Rename(lockPath, stalePath) != nil {
			return false
		}
		_ = os.Remove(stalePath)
		file, err = os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
			return true
		}
	}
	return false
}

func releaseCompletionPodCacheRefreshLock(kubeconfig, kubeContext, namespace string) {
	lockPath, err := completionPodCacheRefreshLockPath(kubeconfig, kubeContext, namespace)
	if err == nil {
		_ = os.Remove(lockPath)
	}
}

func completionPodCachePath(kubeconfig, kubeContext, namespace string) (string, error) {
	key := completionCacheKey("k8s-pods", kubeconfig, completionCacheKubeContext(kubeconfig, kubeContext), namespace)
	dir, err := completionCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".json"), nil
}

func completionPodCacheRefreshLockPath(kubeconfig, kubeContext, namespace string) (string, error) {
	key := completionCacheKey("k8s-pods", kubeconfig, completionCacheKubeContext(kubeconfig, kubeContext), namespace)
	dir, err := completionCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+".lock"), nil
}

// completionCacheKubeContext resolves the effective kube-context for cache
// keying. Keying on the raw --context flag (usually empty) would make two
// clusters share one cache entry: after `kubectl config use-context`, the
// previous cluster's pods would be served as fresh completions.
func completionCacheKubeContext(kubeconfig, kubeContext string) string {
	if kubeContext != "" {
		return kubeContext
	}
	current, err := runtime.KubernetesCurrentContext(kubeconfig)
	if err != nil || current == "" {
		return "default"
	}
	return current
}

func completionCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		// Falling back to a shared, predictable /tmp path invites symlink and
		// cache-poisoning games on multi-user hosts; skip caching instead.
		return "", fmt.Errorf("no user cache directory available")
	}
	return filepath.Join(base, "debux", "completion"), nil
}

// pruneCompletionPodCaches removes cache and lock files past the stale window
// so abandoned (kubeconfig, context, namespace) combinations don't accumulate
// forever. Called from the background refresh process.
func pruneCompletionPodCaches() {
	dir, err := completionCacheDir()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > completionPodCacheStaleFor {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func completionCacheKey(parts ...string) string {
	for i, part := range parts {
		if i == 1 {
			parts[i] = completionKubeconfigCacheKey(part)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func completionKubeconfigCacheKey(kubeconfig string) string {
	if kubeconfig != "" {
		if abs, err := filepath.Abs(kubeconfig); err == nil {
			return abs
		}
		return kubeconfig
	}
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return env
	}
	return "default"
}
