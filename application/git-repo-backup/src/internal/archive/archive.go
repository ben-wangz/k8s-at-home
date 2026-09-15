// Package archive renders deterministic, safety-checked tar.gz mirrors.
// Only directories and regular files enter the archive; symlinks, special
// files, alternates, lock files, hook samples, and cache state never do.
package archive

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
)

// Result carries the verified archive digest and size.
type Result struct {
	SHA256 string
	Size   int64
}

// counter sums bytes streamed through it.
type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

// Create streams mirrorPath into a gzipped tar at destPath with one
// top-level entry <name>.git/. It fsyncs, renames away the .tmp suffix,
// fsyncs the parent, then re-reads the final file to verify the digest and
// size. configBytes replaces the mirror config inside the archive.
func Create(destPath, mirrorPath, name string, level int, configBytes []byte) (Result, error) {
	tmpPath := destPath + ".tmp"
	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "create archive temp file", err)
	}
	digest := sha256.New()
	count := &counter{}
	gz, err := gzip.NewWriterLevel(io.MultiWriter(f, digest, count), level)
	if err != nil {
		f.Close()
		os.Remove(tmpPath)
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "init gzip", err)
	}
	tw := tar.NewWriter(gz)

	err = writeTree(tw, mirrorPath, name, configBytes)
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = gz.Close()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmpPath)
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "write archive", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "finalize archive", err)
	}
	if err := classifyDirSync(safefs.SyncDir(filepath.Dir(destPath))); err != nil {
		return Result{}, err
	}
	result, err := verify(destPath, digest, count.n)
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

// verify re-reads the final archive so the digest covers what is on disk,
// not merely what was buffered.
func verify(destPath string, digest hash.Hash, wrote int64) (Result, error) {
	f, err := os.Open(destPath)
	if err != nil {
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "reopen archive", err)
	}
	defer f.Close()
	rDigest := sha256.New()
	rCount := &counter{}
	if _, err := io.Copy(io.MultiWriter(rDigest, rCount), f); err != nil {
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed, "verify archive", err)
	}
	if rCount.n != wrote {
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed,
			"archive size changed after write", nil)
	}
	got := hex.EncodeToString(rDigest.Sum(nil))
	want := hex.EncodeToString(digest.Sum(nil))
	if got != want {
		return Result{}, observability.WrapSafe(observability.CodeArchiveFailed,
			"archive digest mismatch after write", nil)
	}
	return Result{SHA256: got, Size: rCount.n}, nil
}

func classifyDirSync(err error) error {
	if err == nil {
		return nil
	}
	if err == safefs.ErrDirSyncUnsupported {
		return observability.WrapSafe(observability.CodeCapabilityMissing,
			"directory fsync unsupported; durability depends on the volume", err)
	}
	return observability.WrapSafe(observability.CodeArchiveFailed, "sync archive directory", err)
}
