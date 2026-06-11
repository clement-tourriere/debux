package runtime

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// untarToLocal extracts a tar stream produced by a copy operation. When the
// archive holds a single regular file and dst is not an existing directory,
// the file is written to dst itself (cp semantics); otherwise entries are
// extracted inside dst. Entries that would escape dst — including via
// symlinks — are rejected: the archive comes from a container we should not
// blindly trust with local paths.
func untarToLocal(r io.Reader, dst string) error {
	tr := tar.NewReader(r)

	dstIsDir := false
	if info, err := os.Stat(dst); err == nil && info.IsDir() {
		dstIsDir = true
	}

	entries := 0
	wroteSingleFile := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			if entries == 0 {
				return fmt.Errorf("source produced an empty archive")
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}

		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." || name == "" {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the destination", header.Name)
		}
		entries++

		if wroteSingleFile {
			return fmt.Errorf("destination %s is not a directory but the source contains multiple entries", dst)
		}
		if !dstIsDir && entries == 1 && header.Typeflag == tar.TypeReg {
			if err := writeLocalFile(dst, tr, header.FileInfo().Mode()); err != nil {
				return err
			}
			wroteSingleFile = true
			continue
		}
		if !dstIsDir {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return fmt.Errorf("creating destination %s: %w", dst, err)
			}
			dstIsDir = true
		}

		path := filepath.Join(dst, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, header.FileInfo().Mode().Perm()|0o100); err != nil {
				return fmt.Errorf("creating directory %s: %w", path, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("creating directory for %s: %w", path, err)
			}
			if err := writeLocalFile(path, tr, header.FileInfo().Mode()); err != nil {
				return err
			}
		case tar.TypeSymlink:
			linkTarget := filepath.FromSlash(header.Linkname)
			resolved := linkTarget
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(name), resolved)
			}
			if filepath.IsAbs(linkTarget) || strings.HasPrefix(filepath.Clean(resolved), "..") {
				fmt.Printf("Warning: skipping symlink %s -> %s (points outside the copied tree)\n", name, header.Linkname)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("creating directory for %s: %w", path, err)
			}
			_ = os.Remove(path)
			if err := os.Symlink(linkTarget, path); err != nil {
				return fmt.Errorf("creating symlink %s: %w", path, err)
			}
		default:
			// Devices, FIFOs, hard links: not useful on the developer side.
			continue
		}
	}
}

func writeLocalFile(path string, r io.Reader, mode fs.FileMode) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm()|0o200)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// tarLocalPath streams a local file or directory as a tar archive whose
// entries are rooted at the path's base name.
func tarLocalPath(src string) (io.ReadCloser, error) {
	src = filepath.Clean(src)
	info, err := os.Lstat(src)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", src, err)
	}
	base := filepath.Base(src)
	if base == "/" || base == "." {
		return nil, fmt.Errorf("cannot copy %s: pick a concrete file or directory", src)
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		var werr error
		if info.IsDir() {
			werr = filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(src, path)
				if err != nil {
					return err
				}
				name := base
				if rel != "." {
					name = filepath.ToSlash(filepath.Join(base, rel))
				}
				return addTarEntry(tw, path, name)
			})
		} else {
			werr = addTarEntry(tw, src, base)
		}
		if cerr := tw.Close(); werr == nil {
			werr = cerr
		}
		pw.CloseWithError(werr)
	}()
	return pr, nil
}

func addTarEntry(tw *tar.Writer, path, name string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	link := ""
	if info.Mode()&fs.ModeSymlink != 0 {
		if link, err = os.Readlink(path); err != nil {
			return err
		}
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = name
	if info.IsDir() {
		header.Name += "/"
	}
	if err := tw.WriteHeader(header); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(tw, f)
	return err
}
