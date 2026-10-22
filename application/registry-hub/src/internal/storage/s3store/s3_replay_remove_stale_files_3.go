package s3store

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

func (m *replayManager) removeStaleFiles() error {
	entries, err := os.ReadDir(m.config.directory)
	if err != nil {
		return fmt.Errorf("list S3 replay directory: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), replayFilePrefix) || !entry.Type().IsRegular() {
			continue
		}
		if err := os.Remove(filepath.Join(m.config.directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale S3 replay file: %w", err)
		}
	}
	return nil
}

func (m *replayManager) releaseLock() error {
	if m.lockFile == nil {
		return nil
	}
	unlockErr := unix.Flock(int(m.lockFile.Fd()), unix.LOCK_UN)
	closeErr := m.lockFile.Close()
	m.lockFile = nil
	return errors.Join(unlockErr, closeErr)
}

func maxInt() int { return int(^uint(0) >> 1) }
