package filesystem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"registry-hub/internal/storage"
	"slices"
	"strings"
)

func (s *Store) scanMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	if limit <= 0 {
		limit = 1000
	}
	base := s.root
	if prefix != "" {
		path, err := s.path(prefix + "entry")
		if err != nil {
			return nil, "", err
		}
		base = filepath.Dir(path)
	}
	entries := make([]storage.Entry, 0, limit+1)
	if err := s.walkMetadata(ctx, base, prefix, token, limit+1, &entries); err != nil {
		return nil, "", mapError(err)
	}
	if len(entries) > limit {
		return entries[:limit], entries[limit-1].Key, nil
	}
	return entries, "", nil
}

func (s *Store) walkMetadata(ctx context.Context, dir, prefix, token string, limit int, result *[]storage.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	children, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := func(entry os.DirEntry) string {
		if entry.IsDir() {
			return entry.Name() + "/"
		}
		return entry.Name()
	}
	slices.SortFunc(children, func(a, b os.DirEntry) int { return strings.Compare(name(a), name(b)) })
	for _, child := range children {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(*result) >= limit {
			return nil
		}
		if child.Type()&os.ModeSymlink != 0 || strings.HasPrefix(child.Name(), ".staging-") || strings.HasSuffix(child.Name(), ".lock") {
			continue
		}
		path := filepath.Join(dir, child.Name())
		relative, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if child.IsDir() {
			if key+"/" > token || strings.HasPrefix(token, key+"/") {
				if err := s.walkMetadata(ctx, path, prefix, token, limit, result); err != nil {
					return err
				}
			}
			continue
		}
		if key <= token || !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, _, err := s.Fingerprint(ctx, key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		*result = append(*result, entry)
	}
	return nil
}
