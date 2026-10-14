package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"sort"
	"strings"
	"time"
)

func (s *Store) List(ctx context.Context, prefix, token string, limit int) (result []storage.Entry, nextToken string, returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	base := s.root
	if prefix != "" {
		var err error
		base, err = s.path(strings.TrimSuffix(prefix, "/") + "/entry")
		if err != nil {
			return nil, "", err
		}
		base = filepath.Dir(base)
	}
	var keys []string
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".staging-") || strings.HasSuffix(d.Name(), ".lock") {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasPrefix(key, prefix) && key > token {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Strings(keys)
	if limit <= 0 {
		limit = 1000
	}
	more := len(keys) > limit
	if more {
		keys = keys[:limit]
	}
	entries := make([]storage.Entry, 0, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		path, err := s.path(key)
		if err != nil {
			return nil, "", err
		}
		entry, err := s.head(path, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return nil, "", err
		}
		if err == nil {
			entries = append(entries, entry)
		}
	}
	if more && len(entries) > 0 {
		return entries, entries[len(entries)-1].Key, nil
	}
	return entries, "", nil
}

func (s *Store) Close() error { return nil }

func (s *Store) lock(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") {
		return "", storage.ErrInvalidKey
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\x00\r\n") {
			return "", storage.ErrInvalidKey
		}
	}
	path := filepath.Join(s.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", storage.ErrInvalidKey
	}
	current := s.root
	parts := strings.Split(filepath.FromSlash(key), string(os.PathSeparator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() {
			return "", storage.ErrInvalidKey
		}
	}
	return path, nil
}

func (s *Store) head(path, key string) (storage.Entry, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return storage.Entry{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.Entry{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return storage.Entry{}, err
	}
	version, err := hashReader(file)
	if err != nil {
		return storage.Entry{}, err
	}
	return storage.Entry{Key: key, Version: version, Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

// CleanupOrphans removes stale FileSystem staging files left by interrupted writes.
func (s *Store) CleanupOrphans(ctx context.Context, olderThan time.Duration, limit int) (removed int, returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if olderThan <= 0 {
		return 0, errors.New("orphan retention must be positive")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	cutoff := time.Now().Add(-olderThan)
	s.mu.Lock()
	defer s.mu.Unlock()
	paths := make([]string, 0, limit)
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), ".staging-") {
			return nil
		}
		info, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		paths = append(paths, path)
		if len(paths) >= limit {
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipAll) {
		return 0, err
	}
	removed = 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return removed, err
		}
		if err := s.ops.syncDir(filepath.Dir(path)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func hashReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
