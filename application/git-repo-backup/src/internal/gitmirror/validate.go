package gitmirror

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func writeMirrorConfig(dir string, data []byte) error {
	return os.WriteFile(filepath.Join(dir, "config"), data, 0o600)
}

// cacheState persists only a fingerprint of the source URL, never the URL.
type cacheState struct {
	URLFingerprint string `json:"urlFingerprint"`
}

func fingerprint(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// ValidateStructure verifies a cache candidate is a plain bare mirror: bare
// config, no shallow file, no alternates, and no symlinks anywhere in the
// tree. Anything else forces a rebuild instead of being trusted.
func (r *Runner) ValidateStructure(ctx context.Context, dir string) error {
	out, err := r.Run(ctx, "-C", dir, "config", "--get", "core.bare")
	if err != nil || strings.TrimSpace(out) != "true" {
		return fmt.Errorf("cache entry is not a bare repository")
	}
	if _, err := os.Lstat(filepath.Join(dir, "shallow")); err == nil {
		return fmt.Errorf("cache entry is shallow")
	}
	if _, err := os.Lstat(filepath.Join(dir, "objects", "info", "alternates")); err == nil {
		return fmt.Errorf("cache entry uses alternates")
	}
	return rejectSymlinks(dir)
}

// rejectSymlinks walks dir and fails on any symlink entry.
func rejectSymlinks(dir string) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink inside mirror tree")
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("non-regular file inside mirror tree")
		}
		return nil
	})
}

// readCacheState loads the state file, returning a zero value when absent.
func readCacheState(statePath string) (cacheState, error) {
	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return cacheState{}, nil
		}
		return cacheState{}, err
	}
	var st cacheState
	if err := json.Unmarshal(data, &st); err != nil {
		return cacheState{}, nil // corrupt state behaves like a mismatch
	}
	return st, nil
}

func writeCacheState(statePath, urlFingerprint string) error {
	data, err := json.Marshal(cacheState{URLFingerprint: urlFingerprint})
	if err != nil {
		return err
	}
	return os.WriteFile(statePath, data, 0o600)
}
