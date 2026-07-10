package runtime

import (
	"archive/tar"
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FuzzUntarToLocal exercises extraction with archives a hostile container
// could produce. os.Root enforces containment; the fuzz target guards against
// panics and hangs in the header/path handling around it.
func FuzzUntarToLocal(f *testing.F) {
	seed := func(build func(tw *tar.Writer)) []byte {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		build(tw)
		_ = tw.Close()
		return buf.Bytes()
	}

	f.Add(seed(func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o555})
		_ = tw.WriteHeader(&tar.Header{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5})
		_, _ = tw.Write([]byte("hello"))
	}))
	f.Add(seed(func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0})
	}))
	f.Add(seed(func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o777})
	}))
	f.Add(seed(func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Name: "a", Typeflag: tar.TypeLink, Linkname: "../outside", Mode: 0o644})
	}))
	f.Add([]byte("not a tar archive"))

	f.Fuzz(func(t *testing.T, data []byte) {
		parent := t.TempDir()
		dst := filepath.Join(parent, "dst")
		if err := os.Mkdir(dst, 0o755); err != nil {
			t.Fatal(err)
		}
		canary := filepath.Join(parent, "canary")

		// Extraction may faithfully restore read-only directory modes;
		// make everything writable again so TempDir cleanup succeeds.
		defer func() {
			_ = filepath.WalkDir(dst, func(path string, d fs.DirEntry, err error) error {
				if err == nil && d.IsDir() {
					_ = os.Chmod(path, 0o755)
				}
				return nil
			})
		}()

		// Errors are expected for garbage input; escapes and panics are not.
		_ = untarToLocal(bytes.NewReader(data), dst)

		if _, err := os.Lstat(canary); !os.IsNotExist(err) {
			t.Fatalf("extraction escaped the destination: created %s", canary)
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "dst" {
				t.Fatalf("extraction escaped the destination: created %s", e.Name())
			}
		}
	})
}

// FuzzParseTarget guards the target-URI parser (scheme splitting, %-escapes,
// @context handling) against panics and inconsistent results.
func FuzzParseTarget(f *testing.F) {
	for _, s := range []string{
		"my-app",
		"docker://my-app",
		"docker://",
		"podman://db",
		"compose://web",
		"compose://proj/web",
		"containerd://x",
		"k8s://",
		"k8s://pod",
		"k8s://ns/pod",
		"k8s://ns/pod/ctr",
		"k8s://@ctx/ns/pod/ctr",
		"k8s://@arn%3Aaws%3Aeks%3Aus-west-2%3A123%3Acluster%2Fpreprod/ns/pod",
		"k8s://ns%2Fpod",
		"k8s://@/",
		"k8s://a/b/c/d/e",
		"weird://thing",
		"://",
		"k8s://@ctx//pod",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		target, err := ParseTarget(raw)
		if err != nil {
			return
		}
		if target == nil {
			t.Fatal("nil target without error")
		}
		if target.Runtime == "" {
			t.Fatalf("parsed target of %q has empty runtime: %#v", raw, target)
		}
	})
}
