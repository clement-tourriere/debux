package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

// EnsureImage pulls the image if it's not already present locally.
func EnsureImage(ctx context.Context, cli *client.Client, ref string) error {
	_, _, err := cli.ImageInspectWithRaw(ctx, ref)
	if err == nil {
		return nil // image already present
	}

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
