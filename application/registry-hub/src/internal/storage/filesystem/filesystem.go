package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"sync"
)

type Store struct {
	root string
	mu   sync.Mutex
	ops  fileOperations
}

type fileOperations struct {
	syncFile func(*os.File) error
	rename   func(string, string) error
	syncDir  func(string) error
}

func New(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("filesystem storage root must be absolute")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: resolved, ops: fileOperations{
		syncFile: func(file *os.File) error { return file.Sync() },
		rename:   os.Rename,
		syncDir:  syncDir,
	}}, nil
}

func (s *Store) Get(ctx context.Context, key string) (result storage.Object, returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return storage.Object{}, err
	}
	path, err := s.path(key)
	if err != nil {
		return storage.Object{}, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return storage.Object{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.Object{}, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return storage.Object{}, err
	}
	version, err := hashReader(file)
	if err != nil {
		_ = file.Close()
		return storage.Object{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return storage.Object{}, err
	}
	return storage.Object{Body: file, Size: info.Size(), Version: version, ModTime: info.ModTime().UnixNano()}, nil
}

func (s *Store) Head(ctx context.Context, key string) (storage.Entry, error) {
	obj, err := s.Get(ctx, key)
	if err != nil {
		return storage.Entry{}, err
	}
	_ = obj.Body.Close()
	return storage.Entry{Key: key, Version: obj.Version, Size: obj.Size, ModTime: obj.ModTime}, nil
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (result storage.Entry, returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	path, err := s.path(key)
	if err != nil {
		return storage.Entry{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return storage.Entry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lock(ctx, path)
	if err != nil {
		return storage.Entry{}, err
	}
	defer unlock()
	current, currentErr := s.head(path, key)
	if currentErr != nil && !errors.Is(currentErr, storage.ErrNotFound) {
		return storage.Entry{}, currentErr
	}
	if options.IfAbsent && currentErr == nil {
		return current, storage.ErrAlreadyExists
	}
	if options.IfMatch != "" && (currentErr != nil || current.Version != options.IfMatch) {
		return storage.Entry{}, storage.ErrConditionFailed
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".staging-")
	if err != nil {
		return storage.Entry{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(body, size+1))
	if err != nil {
		_ = tmp.Close()
		return storage.Entry{}, err
	}
	if written != size {
		_ = tmp.Close()
		return storage.Entry{}, errors.New("storage stream length mismatch")
	}
	if err := s.ops.syncFile(tmp); err != nil {
		_ = tmp.Close()
		return storage.Entry{}, err
	}
	if err := tmp.Close(); err != nil {
		return storage.Entry{}, err
	}
	if err := ctx.Err(); err != nil {
		return storage.Entry{}, err
	}
	if err := s.ops.rename(tmpName, path); err != nil {
		return storage.Entry{}, err
	}
	if err := s.ops.syncDir(filepath.Dir(path)); err != nil {
		return storage.Entry{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return storage.Entry{}, err
	}
	return storage.Entry{Key: key, Version: hex.EncodeToString(hash.Sum(nil)), Size: written, ModTime: info.ModTime().UnixNano()}, nil
}

func (s *Store) Delete(ctx context.Context, key string) (returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return storage.ErrNotFound
	} else if err != nil {
		return err
	}
	unlock, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return storage.ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.ops.syncDir(filepath.Dir(path))
}

func (s *Store) DeleteIfMatch(ctx context.Context, key, version string) (returnErr error) {
	defer func() { returnErr = mapError(returnErr) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.path(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		return storage.ErrNotFound
	} else if err != nil {
		return err
	}
	unlock, err := s.lock(ctx, path)
	if err != nil {
		return err
	}
	defer unlock()
	entry, err := s.head(path, key)
	if err != nil {
		return err
	}
	if entry.Version != version {
		return storage.ErrConditionFailed
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return s.ops.syncDir(filepath.Dir(path))
}
