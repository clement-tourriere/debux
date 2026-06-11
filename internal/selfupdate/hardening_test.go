package selfupdate

import "testing"

func TestCompareVersionsBuildMetadata(t *testing.T) {
	// Build metadata may contain hyphens and must not parse as a prerelease.
	if got := compareVersions("1.0.0+build-x", "1.0.0"); got != 0 {
		t.Fatalf("1.0.0+build-x vs 1.0.0 = %d, want 0", got)
	}
	if got := compareVersions("v0.10.0", "v0.9.0"); got != 1 {
		t.Fatalf("v0.10.0 vs v0.9.0 = %d, want 1 (numeric compare)", got)
	}
	if got := compareVersions("1.0.0-rc.1", "1.0.0"); got != -1 {
		t.Fatalf("prerelease should sort before release, got %d", got)
	}
}

func TestIsOutdatedWhenCurrentIsNewer(t *testing.T) {
	if isOutdated("v1.2.0", "v1.1.0") {
		t.Fatal("a newer local build is not outdated")
	}
	if !isOutdated("dev", "v1.0.0") {
		t.Fatal("dev builds are always considered outdated")
	}
}

func TestIsHomebrewManagedPath(t *testing.T) {
	managed := []string{
		"/opt/homebrew/Caskroom/debux/0.5.3/debux",
		"/usr/local/Cellar/debux/0.5.3/bin/debux",
		"/home/linuxbrew/.linuxbrew/bin/debux",
	}
	for _, path := range managed {
		if !isHomebrewManagedPath(path) {
			t.Errorf("%s should be detected as Homebrew-managed", path)
		}
	}
	unmanaged := []string{
		"/Users/me/.local/bin/debux",
		"/usr/local/bin/debux",
	}
	for _, path := range unmanaged {
		if isHomebrewManagedPath(path) {
			t.Errorf("%s should not be detected as Homebrew-managed", path)
		}
	}
}
