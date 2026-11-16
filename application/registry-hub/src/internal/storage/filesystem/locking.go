package filesystem

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lockDirectory = ".registry-hub-locks"
	lockStripes   = 4096
)

func initializeLocks(root string) error {
	path := filepath.Join(root, lockDirectory)
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("filesystem lock pool must be a directory, not a symlink or file")
	}
	return nil
}

func stripeName(key string) string {
	digest := sha256.Sum256([]byte(key))
	stripe := (int(digest[0])<<8 | int(digest[1])) % lockStripes
	return fmt.Sprintf("%04x.lock", stripe)
}

func (s *Store) lock(ctx context.Context, path string) (func(), error) {
	root, err := os.Open(s.root)
	if err != nil {
		return nil, err
	}
	if err := flockContext(ctx, root, unix.LOCK_SH); err != nil {
		_ = root.Close()
		return nil, err
	}
	key, err := filepath.Rel(s.root, path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	lockPath := filepath.Join(s.root, lockDirectory, stripeName(filepath.ToSlash(key)))
	fd, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if err := flockContext(ctx, file, unix.LOCK_EX); err != nil {
		_ = file.Close()
		_ = root.Close()
		return nil, err
	}
	return func() {
		_ = file.Close()
		_ = unix.Flock(int(root.Fd()), unix.LOCK_UN)
		// Directory creation holds a shared root lock; pruning never waits for writers.
		if unix.Flock(int(root.Fd()), unix.LOCK_EX|unix.LOCK_NB) == nil {
			s.pruneEmptyParents(filepath.Dir(path))
		}
		_ = root.Close()
	}, nil
}

func flockContext(ctx context.Context, file *os.File, mode int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) pruneEmptyParents(directory string) {
	for directory != s.root {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return
		}
		directory = parent
	}
}
