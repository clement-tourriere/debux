package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// EnsureImage pulls the image if it's not already present locally.
func EnsureImage(ctx context.Context, cli *client.Client, ref string) error {
	return EnsureImageWithPolicy(ctx, cli, ref, "")
}

// EnsureImageWithPolicy ensures a Docker image according to a Kubernetes-style
// pull policy: Always, IfNotPresent (default), or Never.
func EnsureImageWithPolicy(ctx context.Context, cli *client.Client, ref, pullPolicy string) error {
	policy := strings.ToLower(strings.TrimSpace(pullPolicy))
	switch policy {
	case "", "ifnotpresent":
		if ImageExists(ctx, cli, ref) {
			return nil
		}
		return PullImage(ctx, cli, ref)
	case "always":
		return PullImage(ctx, cli, ref)
	case "never":
		if ImageExists(ctx, cli, ref) {
			return nil
		}
		return fmt.Errorf("image %q is not present locally and pull policy is Never", ref)
	default:
		return fmt.Errorf("invalid pull policy %q: expected Always, IfNotPresent, or Never", pullPolicy)
	}
}

// ImageExists reports whether ref is present locally.
func ImageExists(ctx context.Context, cli *client.Client, ref string) bool {
	_, err := cli.ImageInspect(ctx, ref)
	return err == nil
}

// PullImage pulls ref from a registry.
func PullImage(ctx context.Context, cli *client.Client, ref string) error {
	fmt.Printf("Pulling image %s...\n", ref)
	reader, err := cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image: %w", err)
	}
	defer func() { _ = reader.Close() }()

	// Consume the pull output (docker requires reading the response)
	dec := json.NewDecoder(reader)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading pull response: %w", err)
		}
		if status, ok := msg["status"].(string); ok {
			if progress, ok := msg["progress"].(string); ok && progress != "" {
				fmt.Printf("\r  %s %s", status, progress)
			}
		}
	}
	fmt.Println()

	return nil
}
