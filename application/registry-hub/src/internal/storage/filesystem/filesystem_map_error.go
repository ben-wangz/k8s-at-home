package filesystem

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"registry-hub/internal/storage"
)

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(storage.ErrTimeout, err)
	}
	if errors.Is(err, os.ErrPermission) || errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return errors.Join(storage.ErrPermissionDenied, err)
	}
	if errors.Is(err, unix.EIO) || errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.EROFS) || errors.Is(err, unix.ESTALE) {
		return errors.Join(storage.ErrUnavailable, err)
	}
	return err
}
