package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests run the real update pipeline — resolve tag, download, verify
// checksum (and signature policy), extract, smoke-test, bind version, replace
// binary — against an httptest release server. The downloaded "binary" is a
// shell script so the smoke test and version binding actually execute it.

const testTag = "v9.9.9"

func fakeBinary(version string) []byte {
	return fmt.Appendf(nil, "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo \"debux %s\"; else echo usage; fi\nexit 0\n", version)
}

func makeArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "debux", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer serves a GitHub-shaped release: the latest-release API
// endpoint plus download assets. Assets maps file name to content; a missing
// name 404s like a stripped asset would.
func releaseServer(t *testing.T, repo string, assets map[string][]byte) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+repo+"/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"tag_name":%q}`, testTag)
	})
	mux.HandleFunc("/"+repo+"/releases/download/"+testTag+"/", func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Base(r.URL.Path)
		content, ok := assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(content)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	oldAPI, oldDownload := apiBaseURL, downloadBaseURL
	apiBaseURL, downloadBaseURL = server.URL, server.URL
	t.Cleanup(func() { apiBaseURL, downloadBaseURL = oldAPI, oldDownload })
	return server
}

func checksumsFor(archive string, content []byte) []byte {
	sum := sha256.Sum256(content)
	return fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(sum[:]), archive)
}

func testAssets(t *testing.T, binaryVersion string) map[string][]byte {
	t.Helper()
	archive := archiveName(runtime.GOOS, runtime.GOARCH)
	content := makeArchive(t, fakeBinary(binaryVersion))
	return map[string][]byte{
		archive:         content,
		"checksums.txt": checksumsFor(archive, content),
	}
}

// withoutCosign ensures the signature policy stays out of the way (and out of
// dependence on the developer machine) unless a test opts in via fakeCosign.
func withoutCosign(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func fakeCosign(t *testing.T, exitCode int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, "cosign"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func installTarget(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "debux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunUpdatesEndToEnd(t *testing.T) {
	withoutCosign(t)
	releaseServer(t, "test/repo", testAssets(t, "9.9.9"))
	target := installTarget(t)

	result, err := Run(t.Context(), Options{
		Repo:           "test/repo",
		CurrentVersion: "0.0.1",
		InstallPath:    target,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Updated || result.Latest != testTag {
		t.Fatalf("result = %+v, want Updated with latest %s", result, testTag)
	}

	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), "9.9.9") {
		t.Fatalf("install path was not replaced with the downloaded binary: %q", installed)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
}

func TestRunUpToDateSkipsDownload(t *testing.T) {
	withoutCosign(t)
	releaseServer(t, "test/repo", nil) // no assets: a download attempt would fail

	result, err := Run(t.Context(), Options{
		Repo:           "test/repo",
		CurrentVersion: "9.9.9",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.UpToDate || result.Updated {
		t.Fatalf("result = %+v, want UpToDate without update", result)
	}
}

func TestRunChecksumMismatchKeepsExistingBinary(t *testing.T) {
	withoutCosign(t)
	assets := testAssets(t, "9.9.9")
	assets["checksums.txt"] = checksumsFor(archiveName(runtime.GOOS, runtime.GOARCH), []byte("tampered"))
	releaseServer(t, "test/repo", assets)
	target := installTarget(t)

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: target})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	content, _ := os.ReadFile(target)
	if !strings.Contains(string(content), "old") {
		t.Fatal("a failed update must not touch the existing binary")
	}
}

func TestRunMissingChecksumEntryFails(t *testing.T) {
	withoutCosign(t)
	assets := testAssets(t, "9.9.9")
	assets["checksums.txt"] = []byte("deadbeef  some-other-file.tar.gz\n")
	releaseServer(t, "test/repo", assets)

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("expected missing-entry failure, got %v", err)
	}
}

func TestRunArchiveWithoutBinaryFails(t *testing.T) {
	withoutCosign(t)
	archive := archiveName(runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("hi"))
	_ = tw.Close()
	_ = gz.Close()
	releaseServer(t, "test/repo", map[string][]byte{
		archive:         buf.Bytes(),
		"checksums.txt": checksumsFor(archive, buf.Bytes()),
	})

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "did not contain debux") {
		t.Fatalf("expected missing-binary failure, got %v", err)
	}
}

func TestRunBinaryVersionMismatchFails(t *testing.T) {
	// Checksums prove the assets belong to *some* release; the version bind
	// must reject assets from a different (older) release served under the
	// requested tag's URLs.
	withoutCosign(t)
	releaseServer(t, "test/repo", testAssets(t, "1.0.0"))

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("expected version-bind failure, got %v", err)
	}
}

func TestRunBinaryVersionSubstringCollisionFails(t *testing.T) {
	// Exact binding matters: 19.9.90 contains the requested 9.9.9 as text but
	// is a different release.
	withoutCosign(t)
	releaseServer(t, "test/repo", testAssets(t, "19.9.90"))

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "tampered") {
		t.Fatalf("expected exact version-bind failure, got %v", err)
	}
}

func TestRunFailsClosedWhenSignatureAssetsMissing(t *testing.T) {
	// With cosign present, a release without .sig/.pem must abort: an
	// attacker able to tamper with assets could also strip the signatures.
	fakeCosign(t, 0)
	releaseServer(t, "test/repo", testAssets(t, "9.9.9")) // no .sig/.pem assets

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "expected to be signed") {
		t.Fatalf("expected fail-closed signature error, got %v", err)
	}
}

func TestRunAllowUnsignedOptOut(t *testing.T) {
	fakeCosign(t, 0)
	t.Setenv("DEBUX_UPDATE_ALLOW_UNSIGNED", "1")
	releaseServer(t, "test/repo", testAssets(t, "9.9.9"))
	target := installTarget(t)

	result, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: target})
	if err != nil {
		t.Fatalf("opt-out should fall back to checksum-only verification, got %v", err)
	}
	if !result.Updated {
		t.Fatalf("result = %+v, want an update", result)
	}
}

func TestRunSignatureVerificationFailureAborts(t *testing.T) {
	fakeCosign(t, 1) // cosign verify-blob rejects the signature
	assets := testAssets(t, "9.9.9")
	assets["checksums.txt.sig"] = []byte("sig")
	assets["checksums.txt.pem"] = []byte("pem")
	releaseServer(t, "test/repo", assets)

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: installTarget(t)})
	if err == nil || !strings.Contains(err.Error(), "verifying checksums signature") {
		t.Fatalf("expected signature verification failure, got %v", err)
	}
}

func TestRunRefusesHomebrewManagedInstall(t *testing.T) {
	withoutCosign(t)
	releaseServer(t, "test/repo", testAssets(t, "9.9.9"))

	cellar := filepath.Join(t.TempDir(), "Cellar", "debux", "1.0.0", "bin")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(cellar, "debux")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "debux")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	_, err := Run(t.Context(), Options{Repo: "test/repo", CurrentVersion: "0.0.1", InstallPath: link})
	if err == nil || !strings.Contains(err.Error(), "brew upgrade") {
		t.Fatalf("expected Homebrew guard, got %v", err)
	}
}
