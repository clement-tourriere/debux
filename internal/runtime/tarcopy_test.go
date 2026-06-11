package runtime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarRoundtrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	reader, err := tarLocalPath(src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()

	dst := t.TempDir()
	if err := untarToLocal(reader, dst); err != nil {
		t.Fatal(err)
	}

	base := filepath.Base(src)
	got, err := os.ReadFile(filepath.Join(dst, base, "sub", "b.txt"))
	if err != nil || string(got) != "world" {
		t.Fatalf("roundtrip content = %q, err %v", got, err)
	}
}

func TestUntarSingleFileToExplicitPath(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	content := []byte("data")
	if err := tw.WriteHeader(&tar.Header{Name: "remote-name.log", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()

	dst := filepath.Join(t.TempDir(), "local-name.log")
	if err := untarToLocal(&buf, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "data" {
		t.Fatalf("single-file copy = %q, err %v", got, err)
	}
}

func TestUntarRejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644, Size: 0}); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()

	err := untarToLocal(&buf, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("expected traversal rejection, got %v", err)
	}
}

func TestUntarEmptyArchiveErrors(t *testing.T) {
	var buf bytes.Buffer
	_ = tar.NewWriter(&buf).Close()
	if err := untarToLocal(&buf, t.TempDir()); err == nil {
		t.Fatal("empty archive should error (remote path probably did not exist)")
	}
}
