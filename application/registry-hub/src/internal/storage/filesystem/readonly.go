package filesystem

import (
	"errors"
	"os"
	"path/filepath"
)

func OpenReadOnly(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("inspection root must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("inspection root must be a directory")
	}
	return &Store{root: resolved, readOnly: true}, nil
}
