package store

import "testing"

func TestToolVolumeDoesNotMountOverSystemTools(t *testing.T) {
	v := ToolVolumesForImage("sha256:0123456789abcdef", "abcdef0123456789")
	mounts := v.Mounts()
	if len(mounts) != 1 || mounts[0].Target != "/var/lib/debux" || mounts[0].Source != v.Data {
		t.Fatalf("bad current toolbox mounts: %+v", mounts)
	}
	if v == ToolVolumesForImage("sha256:different-image", "abcdef0123456789") || v == ToolVolumesForImage("sha256:0123456789abcdef", "different-scope") {
		t.Fatal("images or security identities share a mutable tool volume")
	}
	legacy := VolumesForImage("sha256:0123456789abcdef").Mounts()
	if len(legacy) != 2 || legacy[0].Target != "/nix/store" || legacy[1].Target != "/nix/var" {
		t.Fatalf("legacy image support lost: %+v", legacy)
	}
}

func TestVolumesForImage(t *testing.T) {
	volumes := VolumesForImage("sha256:0123456789abcdef")
	if volumes.NixStore != "debux-nix-store-0123456789ab" {
		t.Fatalf("NixStore = %q", volumes.NixStore)
	}
	if volumes.NixVar != "debux-nix-var-0123456789ab" {
		t.Fatalf("NixVar = %q", volumes.NixVar)
	}
}
