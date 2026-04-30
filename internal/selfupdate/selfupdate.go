package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultRepo = "clement-tourriere/debux"

type Options struct {
	Repo           string
	CurrentVersion string
	TargetVersion  string
	InstallPath    string
	CheckOnly      bool
	Force          bool
	Stdout         io.Writer
}

type Result struct {
	Current     string
	Latest      string
	InstallPath string
	Updated     bool
	CheckOnly   bool
	UpToDate    bool
	Archive     string
}

type releaseResponse struct {
	TagName string `json:"tag_name"`
}

func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Repo == "" {
		opts.Repo = DefaultRepo
	}
	if opts.CurrentVersion == "" {
		opts.CurrentVersion = "dev"
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}

	tag, err := resolveTag(ctx, opts.Repo, opts.TargetVersion)
	if err != nil {
		return Result{}, err
	}

	archive, err := supportedArchiveName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Current:     opts.CurrentVersion,
		Latest:      tag,
		InstallPath: opts.InstallPath,
		CheckOnly:   opts.CheckOnly,
		Archive:     archive,
	}

	if opts.TargetVersion == "" && !opts.Force && !isOutdated(opts.CurrentVersion, tag) {
		result.UpToDate = true
		return result, nil
	}
	if opts.TargetVersion != "" && !opts.Force && sameVersion(opts.CurrentVersion, tag) {
		result.UpToDate = true
		return result, nil
	}
	if opts.CheckOnly {
		return result, nil
	}

	if opts.InstallPath == "" {
		path, err := os.Executable()
		if err != nil {
			return Result{}, fmt.Errorf("locating current executable: %w", err)
		}
		opts.InstallPath = path
		result.InstallPath = path
	}

	_, _ = fmt.Fprintf(opts.Stdout, "Downloading debux %s for %s/%s...\n", tag, runtime.GOOS, runtime.GOARCH)
	binaryPath, err := downloadReleaseBinary(ctx, opts.Repo, tag, result.Archive)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = os.RemoveAll(filepath.Dir(binaryPath)) }()

	if err := replaceExecutable(binaryPath, opts.InstallPath); err != nil {
		return Result{}, fmt.Errorf("installing %s: %w", opts.InstallPath, err)
	}
	postInstallFixups(opts.InstallPath)

	result.Updated = true
	return result, nil
}

func resolveTag(ctx context.Context, repo, targetVersion string) (string, error) {
	if targetVersion != "" && targetVersion != "latest" {
		return normalizeTag(targetVersion), nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "debux-updater")

	resp, err := httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("checking latest release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return "", fmt.Errorf("no GitHub Releases found for %s yet; create a release tag before using debux update", repo)
		}
		return "", fmt.Errorf("checking latest release: GitHub returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", fmt.Errorf("decoding latest release: %w", err)
	}
	if release.TagName == "" {
		return "", fmt.Errorf("latest release did not include a tag name")
	}
	return release.TagName, nil
}

func downloadReleaseBinary(ctx context.Context, repo, tag, archive string) (string, error) {
	tmp, err := os.MkdirTemp("", "debux-update-*")
	if err != nil {
		return "", fmt.Errorf("creating temp dir: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmp)
		}
	}()

	baseURL := fmt.Sprintf("https://github.com/%s/releases/download/%s", repo, tag)
	archivePath := filepath.Join(tmp, archive)
	if err := downloadFile(ctx, baseURL+"/"+archive, archivePath); err != nil {
		return "", err
	}

	checksumsPath := filepath.Join(tmp, "checksums.txt")
	if err := downloadFile(ctx, baseURL+"/checksums.txt", checksumsPath); err == nil {
		if err := verifyChecksum(archivePath, checksumsPath, archive); err != nil {
			return "", err
		}
	}

	binaryPath := filepath.Join(tmp, "debux")
	if err := extractBinary(archivePath, binaryPath); err != nil {
		return "", err
	}
	cleanup = false
	return binaryPath, nil
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "debux-updater")

	resp, err := httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("downloading %s: GitHub returned %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
	}

	out, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

func verifyChecksum(archivePath, checksumsPath, archive string) error {
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("reading checksums: %w", err)
	}

	var expected string
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == archive {
			expected = fields[0]
			break
		}
	}
	if expected == "" {
		return fmt.Errorf("checksums.txt does not contain %s", archive)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive for checksum: %w", err)
	}
	defer func() { _ = f.Close() }()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("hashing archive: %w", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", archive, expected, actual)
	}
	return nil
}

func extractBinary(archivePath, binaryPath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("reading gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg || filepath.Base(header.Name) != "debux" {
			continue
		}

		out, err := os.OpenFile(binaryPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return fmt.Errorf("creating extracted binary: %w", err)
		}
		if _, err := io.Copy(out, tr); err != nil {
			_ = out.Close()
			return fmt.Errorf("extracting debux: %w", err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("closing extracted binary: %w", err)
		}
		return nil
	}

	return fmt.Errorf("archive did not contain debux")
}

func replaceExecutable(src, dst string) error {
	if dst == "" {
		return fmt.Errorf("empty install path")
	}

	mode := os.FileMode(0o755)
	if info, err := os.Stat(dst); err == nil {
		mode = info.Mode().Perm()
	}

	dir := filepath.Dir(dst)
	tmp, err := os.CreateTemp(dir, ".debux-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Chmod(mode | 0o111); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func postInstallFixups(path string) {
	if runtime.GOOS != "darwin" {
		return
	}
	_ = exec.Command("xattr", "-c", path).Run()
	_ = exec.Command("codesign", "--force", "--sign", "-", path).Run()
}

func supportedArchiveName(goos, goarch string) (string, error) {
	switch goos {
	case "darwin", "linux":
	default:
		return "", fmt.Errorf("unsupported OS for debux update: %s", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture for debux update: %s", goarch)
	}
	return archiveName(goos, goarch), nil
}

func archiveName(goos, goarch string) string {
	return fmt.Sprintf("debux_%s_%s.tar.gz", goos, goarch)
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

func normalizeTag(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || version == "latest" {
		return version
	}
	if strings.HasPrefix(version, "v") {
		return version
	}
	return "v" + version
}

func sameVersion(current, target string) bool {
	return normalizeVersion(current) == normalizeVersion(target)
}

func isOutdated(current, latest string) bool {
	if current == "" || current == "dev" {
		return true
	}
	return compareVersions(current, latest) < 0
}

func compareVersions(a, b string) int {
	an := normalizeVersion(a)
	bn := normalizeVersion(b)
	if an == "dev" && bn != "dev" {
		return -1
	}
	if an != "dev" && bn == "dev" {
		return 1
	}

	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return strings.Compare(normalizeVersion(a), normalizeVersion(b))
	}
	for i := 0; i < len(av) || i < len(bv); i++ {
		ai, bi := 0, 0
		if i < len(av) {
			ai = av[i]
		}
		if i < len(bv) {
			bi = bv[i]
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
	}
	return 0
}

func parseVersion(v string) ([]int, bool) {
	v = normalizeVersion(v)
	if v == "" || v == "dev" {
		return nil, false
	}
	main, _, _ := strings.Cut(v, "-")
	parts := strings.Split(main, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, len(out) > 0
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	return v
}
