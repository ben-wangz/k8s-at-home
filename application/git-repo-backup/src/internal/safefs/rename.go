package safefs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// ErrNoReplaceUnsupported reports that the filesystem or kernel lacks
// renameat2(RENAME_NOREPLACE). Callers must fail loudly instead of degrading
// to a non-atomic copy or unconditional rename.
var ErrNoReplaceUnsupported = errors.New("renameat2 RENAME_NOREPLACE unsupported")

// ErrExists reports that the destination already exists.
var ErrExists = os.ErrExist

// RenameNoReplace atomically renames oldpath to newpath only when newpath
// does not exist, using renameat2 with RENAME_NOREPLACE.
func RenameNoReplace(oldpath, newpath string) error {
	err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE)
	switch {
	case err == nil:
		return nil
	case err == unix.EEXIST:
		return ErrExists
	case err == unix.ENOSYS, err == unix.EINVAL, err == unix.ENOTTY:
		return ErrNoReplaceUnsupported
	}
	return os.NewSyscallError("renameat2", err)
}
