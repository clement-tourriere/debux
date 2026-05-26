package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/moby/moby/client"
)

const (
	// Legacy volume names. Kept so `debux store clean` can remove older stores.
	NixStoreVolume = "debux-nix-store"
	NixVarVolume   = "debux-nix-var"
)

// VolumeSet holds the Docker volumes used for a specific debug image.
type VolumeSet struct {
	NixStore string
	NixVar   string
}

// Volumes returns the legacy volume names managed by debux.
func Volumes() []string {
	return []string{NixStoreVolume, NixVarVolume}
}

// VolumesForImage returns image-specific Nix volumes. The image suffix avoids
// mounting an old /nix/store over a rebuilt debug image, which can break the
// image's /bin/sh symlink before the container even starts.
func VolumesForImage(imageID string) VolumeSet {
	suffix := imageIDSuffix(imageID)
	return VolumeSet{
		NixStore: "debux-nix-store-" + suffix,
		NixVar:   "debux-nix-var-" + suffix,
	}
}

func imageIDSuffix(imageID string) string {
	imageID = strings.TrimSpace(imageID)
	imageID = strings.TrimPrefix(imageID, "sha256:")
	if imageID == "" {
		return "unknown"
	}
	if len(imageID) > 12 {
		return imageID[:12]
	}
	return imageID
}

// EnsureVolumes creates the persistent Nix volumes if they don't exist.
func EnsureVolumes(ctx context.Context, cli *client.Client, volumes VolumeSet) error {
	if err := ensureVolume(ctx, cli, volumes.NixStore, "nix-store"); err != nil {
		return err
	}
	if err := ensureVolume(ctx, cli, volumes.NixVar, "nix-var"); err != nil {
		return err
	}
	return nil
}

func ensureVolume(ctx context.Context, cli *client.Client, name, kind string) error {
	_, err := cli.VolumeInspect(ctx, name, client.VolumeInspectOptions{})
	if err == nil {
		return nil
	}

	_, err = cli.VolumeCreate(ctx, client.VolumeCreateOptions{
		Name: name,
		Labels: map[string]string{
			"managed-by":                        "debux",
			"debux.clement-tourriere/kind":      kind,
			"debux.clement-tourriere/store-gen": "image-id",
		},
	})
	if err != nil {
		return fmt.Errorf("creating volume %s: %w", name, err)
	}
	return nil
}

// Clean removes all persistent volumes managed by debux.
func Clean(ctx context.Context) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	f := make(client.Filters).Add("label", "managed-by=debux")
	list, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}

	if len(list.Items) == 0 {
		fmt.Println("No debux store volumes found.")
		return nil
	}

	for _, v := range list.Items {
		if _, err := cli.VolumeRemove(ctx, v.Name, client.VolumeRemoveOptions{Force: true}); err != nil {
			return fmt.Errorf("removing volume %s: %w", v.Name, err)
		}
		fmt.Printf("Removed %s\n", v.Name)
	}
	return nil
}

// Info prints information about the persistent Nix volumes.
func Info(ctx context.Context) error {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	f := make(client.Filters).Add("label", "managed-by=debux")
	list, err := cli.VolumeList(ctx, client.VolumeListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}

	if len(list.Items) == 0 {
		fmt.Println("No debux store volumes found.")
		return nil
	}

	fmt.Println("debux store volumes:")
	for _, v := range list.Items {
		fmt.Printf("  %s (driver: %s, mountpoint: %s)\n", v.Name, v.Driver, v.Mountpoint)
		if v.UsageData != nil {
			fmt.Printf("    size: %d MB, ref count: %d\n", v.UsageData.Size/(1024*1024), v.UsageData.RefCount)
		}
	}
	return nil
}
