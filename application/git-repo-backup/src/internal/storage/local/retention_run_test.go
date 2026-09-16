package local

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"git-repo-backup/internal/retention"
)

// Reproduces the integration observation: fresh staging must survive a
// retention pass while stale staging, trash, and marker-less backups are
// cleaned.
func TestRetentionKeepsFreshStaging(t *testing.T) {
	st, err := New(t.TempDir(), 1, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(context.Background())
	old := time.Now().UTC().Add(-72 * time.Hour).Format("20060102T150405Z")
	fresh := time.Now().UTC().Format("20060102T150405Z")
	for _, name := range []string{old + "-staleowner", fresh + "-freshowner"} {
		if err := os.MkdirAll(filepath.Join(st.root, dirStaging, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(st.root, dirTrash, old+"-staleowner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(st.root, dirBackups, old), 0o700); err != nil {
		t.Fatal(err)
	}
	opts := retention.Options{
		Enabled: true, MaxBackups: 5, MaxAge: 0,
		IncompleteMaxAge: 30 * time.Minute, MaxRunGrace: 20 * time.Minute,
		Now: time.Now().UTC(),
	}
	if err := retention.Run(context.Background(), st, "20990101T000000Z", opts, slog.Default()); err != nil {
		t.Fatalf("retention errored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(st.root, dirStaging, old+"-staleowner")); !os.IsNotExist(err) {
		t.Fatal("stale staging must be cleaned")
	}
	if _, err := os.Stat(filepath.Join(st.root, dirStaging, fresh+"-freshowner")); err != nil {
		t.Fatalf("fresh staging must be kept: %v", err)
	}
}
