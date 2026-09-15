package safefs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ValidateAbsolutePath enforces the shared rules for every configurable local
// path: absolute, not the filesystem root, canonical (no '..', '.', or
// empty segments), no control characters, and valid UTF-8.
func ValidateAbsolutePath(name, path string) error {
	if path == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path", name)
	}
	if path == "/" {
		return fmt.Errorf("%s must not be the filesystem root", name)
	}
	if !utf8.ValidString(path) {
		return fmt.Errorf("%s must be valid UTF-8", name)
	}
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s must not contain control characters", name)
		}
	}
	for i, part := range strings.Split(path, "/") {
		if i == 0 && part == "" {
			continue // leading slash of an absolute path
		}
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%s must be a canonical path without '.', '..', or empty segments", name)
		}
	}
	return nil
}

// EnsureDir creates dir (and parents) with mode and verifies via Lstat that
// the final path itself is a real directory, not a symlink.
func EnsureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s exists but is not a directory", dir)
	}
	return nil
}

// IsEmptyDir reports whether dir exists and contains no entries.
func IsEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// ContainsPath reports whether parent == child or child is inside parent.
func ContainsPath(parent, child string) bool {
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, ensureTrailingSlash(parent))
}

func ensureTrailingSlash(p string) string {
	if strings.HasSuffix(p, string(os.PathSeparator)) {
		return p
	}
	return p + string(os.PathSeparator)
}
