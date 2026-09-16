package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOwnedByCurrentUser guards the platform stat assertion: os.FileInfo
// carries *syscall.Stat_t, and the ownership check must actually evaluate
// it (a wrong type assertion silently rejected every run at preflight).
func TestOwnedByCurrentUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !ownedByCurrentUser(info) {
		t.Fatal("file created by the current uid must be recognized as owned")
	}
}
