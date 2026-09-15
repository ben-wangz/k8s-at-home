package safefs

import (
	"os"
	"path/filepath"
)

// WriteFileDurable creates a new file at path with the given data and mode,
// fsyncs the file, then fsyncs the parent directory. It refuses to overwrite
// an existing file. Directory-sync capability gaps are reported via
// ErrDirSyncUnsupported for the caller to classify.
func WriteFileDurable(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return SyncDir(filepath.Dir(path))
}
