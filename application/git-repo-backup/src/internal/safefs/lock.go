// Package safefs wraps the Linux filesystem primitives the backup relies on:
// non-blocking flock, renameat2(RENAME_NOREPLACE), statfs capacity checks,
// directory fsync, and path-safety validation.
package safefs

import (
	"os"

	"golang.org/x/sys/unix"
)

// ErrLockHeld reports that another process holds the lock.
var ErrLockHeld = os.NewSyscallError("flock LOCK_NB", unix.EWOULDBLOCK)

// Lock is a non-blocking exclusive advisory lock on a lock file. The file is
// never unlinked on release: a vanished lock file would allow two processes
// to lock different inodes.
type Lock struct {
	f *os.File
}

// TryLock opens (creating if needed) path and acquires a non-blocking
// exclusive flock on it. The returned error is ErrLockHeld when another
// holder exists.
func TryLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK || err == unix.EAGAIN {
			return nil, ErrLockHeld
		}
		return nil, os.NewSyscallError("flock", err)
	}
	return &Lock{f: f}, nil
}

// Close releases the lock. The lock file itself is kept.
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unix.Flock(int(l.f.Fd()), unix.LOCK_UN)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return os.NewSyscallError("flock LOCK_UN", err)
	}
	return cerr
}
