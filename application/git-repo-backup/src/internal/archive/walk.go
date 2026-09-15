package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"git-repo-backup/internal/observability"
)

// writeTree walks the mirror deterministically (lexicographic per directory)
// and emits sanitized tar entries rooted at <name>.git/.
func writeTree(tw *tar.Writer, mirrorPath, name string, configBytes []byte) error {
	rootInfo, err := os.Lstat(mirrorPath)
	if err != nil || !rootInfo.IsDir() {
		return observability.WrapSafe(observability.CodeArchiveFailed, "mirror root missing or not a directory", err)
	}
	prefix := name + ".git/"
	if err := writeDirHeader(tw, prefix, rootInfo.ModTime()); err != nil {
		return err
	}
	return filepath.WalkDir(mirrorPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return observability.WrapSafe(observability.CodeArchiveFailed, "walk mirror", err)
		}
		if p == mirrorPath {
			return nil
		}
		rel, err := filepath.Rel(mirrorPath, p)
		if err != nil {
			return observability.WrapSafe(observability.CodeArchiveFailed, "walk mirror", err)
		}
		if rel == "hooks" {
			return filepath.SkipDir // sample hooks never belong in an archive
		}
		if rel == "gc.pid" || strings.HasSuffix(rel, ".gc.lock") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return observability.WrapSafe(observability.CodeArchiveFailed, "stat mirror entry", err)
		}
		entryName := prefix + filepath.ToSlash(rel)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return rejectEntry(rel, "symlink")
		case info.IsDir():
			return writeDirHeader(tw, entryName+"/", info.ModTime())
		case !info.Mode().IsRegular():
			return rejectEntry(rel, "special file")
		}
		if rel == "config" {
			return writeInlineFile(tw, entryName, configBytes, info.ModTime())
		}
		if rel == filepath.Join("objects", "info", "alternates") {
			return rejectEntry(rel, "alternates file")
		}
		return writeFile(tw, p, rel, entryName, info)
	})
}

// rejectEntry fails the archive on anything disallowed: alternates, lock
// files, symlinks, and special files.
func rejectEntry(rel, what string) error {
	if rel == filepath.Join("objects", "info", "alternates") {
		what = "alternates file"
	}
	return observability.WrapSafe(observability.CodeArchiveFailed,
		fmt.Sprintf("mirror contains %s entry", what), nil)
}

func writeDirHeader(tw *tar.Writer, name string, mtime time.Time) error {
	return tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir,
		Name:     name,
		Mode:     0o700,
		ModTime:  mtime,
		Format:   tar.FormatPAX,
	})
}

func writeFile(tw *tar.Writer, diskPath, rel, entryName string, info os.FileInfo) error {
	if strings.HasSuffix(rel, ".lock") {
		return rejectEntry(rel, "in-progress lock file")
	}
	f, err := os.OpenFile(diskPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return observability.WrapSafe(observability.CodeArchiveFailed, "open mirror file", err)
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryName,
		Mode:     0o600,
		Size:     info.Size(),
		ModTime:  info.ModTime(),
		Format:   tar.FormatPAX,
	}); err != nil {
		return observability.WrapSafe(observability.CodeArchiveFailed, "write tar header", err)
	}
	if err := copyExact(tw, f, info.Size()); err != nil {
		return observability.WrapSafe(observability.CodeArchiveFailed,
			"read mirror file (size changed mid-archive?)", err)
	}
	return nil
}

func writeInlineFile(tw *tar.Writer, entryName string, data []byte, mtime time.Time) error {
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg,
		Name:     entryName,
		Mode:     0o600,
		Size:     int64(len(data)),
		ModTime:  mtime,
		Format:   tar.FormatPAX,
	}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// copyExact copies exactly n bytes, failing on short reads (shrunken file)
// and on extra data still available (grown file).
func copyExact(w io.Writer, r io.Reader, n int64) error {
	if _, err := io.CopyN(w, r, n); err != nil {
		return err
	}
	var probe [1]byte
	m, err := r.Read(probe[:])
	if m > 0 {
		return fmt.Errorf("file grew during archive")
	}
	if err != io.EOF {
		return err
	}
	return nil
}
