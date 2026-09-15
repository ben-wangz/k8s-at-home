package safefs

import (
	"errors"
	"os"
	"syscall"
)

// ErrDirSyncUnsupported reports that the filesystem rejects directory fsync.
// Callers log a capability warning; durability after power loss then depends
// on the underlying volume. Real I/O errors are never mapped to this.
var ErrDirSyncUnsupported = errors.New("directory fsync unsupported by filesystem")

// SyncDir fsyncs a directory so its entries are durable. On filesystems that
// explicitly do not support directory fsync it returns ErrDirSyncUnsupported.
func SyncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTTY) {
			return ErrDirSyncUnsupported
		}
		return err
	}
	return nil
}

// SyncFile opens path and fsyncs it, for cases where the writing code no
// longer holds the file handle.
func SyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
