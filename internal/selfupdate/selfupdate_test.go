package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"v1.2.3", "v1.2.4", -1},
		{"1.3.0", "v1.2.9", 1},
		{"v1.2.0", "1.2", 0},
		{"dev", "v1.0.0", -1},
	}
	for _, tt := range tests {
		got := compareVersions(tt.a, tt.b)
		if got < 0 {
			got = -1
		} else if got > 0 {
			got = 1
		}
		if got != tt.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestArchiveName(t *testing.T) {
	if got := archiveName("darwin", "arm64"); got != "debux_darwin_arm64.tar.gz" {
		t.Fatalf("archiveName = %q", got)
	}
}

func TestNormalizeTag(t *testing.T) {
	if got := normalizeTag("1.2.3"); got != "v1.2.3" {
		t.Fatalf("normalizeTag without v = %q", got)
	}
	if got := normalizeTag("v1.2.3"); got != "v1.2.3" {
		t.Fatalf("normalizeTag with v = %q", got)
	}
}
