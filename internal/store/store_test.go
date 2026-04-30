package store

import "testing"

func TestVolumesForImage(t *testing.T) {
	volumes := VolumesForImage("sha256:0123456789abcdef")
	if volumes.NixStore != "debux-nix-store-0123456789ab" {
		t.Fatalf("NixStore = %q", volumes.NixStore)
	}
	if volumes.NixVar != "debux-nix-var-0123456789ab" {
		t.Fatalf("NixVar = %q", volumes.NixVar)
	}
}
