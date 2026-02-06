package store

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

const (
	NixStoreVolume = "debux-nix-store"
	NixVarVolume   = "debux-nix-var"
)

// Volumes returns the list of volume names managed by debux.
func Volumes() []string {
	return []string{NixStoreVolume, NixVarVolume}
}

// EnsureVolumes creates the persistent Nix volumes if they don't exist.
func EnsureVolumes(ctx context.Context, cli *client.Client) error {
	for _, name := range Volumes() {
		if err := ensureVolume(ctx, cli, name); err != nil {
			return err
		}
	}
	return nil
}

func ensureVolume(ctx context.Context, cli *client.Client, name string) error {
	_, err := cli.VolumeInspect(ctx, name)
	if err == nil {
		return nil
	}

	_, err = cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Labels: map[string]string{"managed-by": "debux"},
	})
	if err != nil {
		return fmt.Errorf("creating volume %s: %w", name, err)
	}
	return nil
}

// Clean removes the persistent Nix volumes.
func Clean(ctx context.Context) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	for _, name := range Volumes() {
		if err := cli.VolumeRemove(ctx, name, true); err != nil {
			return fmt.Errorf("removing volume %s: %w", name, err)
		}
	}
	return nil
}

// Info prints information about the persistent Nix volumes.
func Info(ctx context.Context) error {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("connecting to Docker: %w", err)
	}
	defer func() { _ = cli.Close() }()

	f := filters.NewArgs(filters.Arg("label", "managed-by=debux"))
	list, err := cli.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}

	if len(list.Volumes) == 0 {
		fmt.Println("No debux store volumes found.")
		return nil
	}

	fmt.Println("debux store volumes:")
	for _, v := range list.Volumes {
		fmt.Printf("  %s (driver: %s, mountpoint: %s)\n", v.Name, v.Driver, v.Mountpoint)
		if v.UsageData != nil {
			fmt.Printf("    size: %d MB, ref count: %d\n", v.UsageData.Size/(1024*1024), v.UsageData.RefCount)
		}
	}
	return nil
}
