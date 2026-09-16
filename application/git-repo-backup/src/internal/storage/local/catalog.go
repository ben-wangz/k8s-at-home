package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
	"git-repo-backup/internal/storage"
)

// ListBackups recognizes backups/<id> entries: valid IDs only. Unrelated
// names are ignored entirely.
func (s *Store) ListBackups(_ context.Context) ([]storage.Backup, error) {
	entries, err := os.ReadDir(s.backupsDir())
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed, "list backups", err)
	}
	var out []storage.Backup
	for _, e := range entries {
		id := e.Name()
		startedAt, err := storage.ParseBackupID(id)
		if err != nil {
			continue // unrelated name; never considered, never deleted
		}
		b := storage.Backup{ID: id, StartedAt: startedAt}
		dir := filepath.Join(s.backupsDir(), id)
		switch classifyComplete(dir) {
		case classComplete:
			b.Complete = true
		case classAnomaly:
			b.Anomaly = true
		case classIncomplete:
			b.Complete = false
		}
		out = append(out, b)
	}
	return out, nil
}

type completeness uint8

const (
	classIncomplete completeness = iota
	classComplete
	classAnomaly
)

// classifyComplete checks the marker and small metadata structure. Marker
// presence without a matching valid manifest is an anomaly: conservative,
// never deleted, and surfaced as a retention error.
func classifyComplete(dir string) completeness {
	markerPath := filepath.Join(dir, manifest.MarkerName)
	if _, err := os.Lstat(markerPath); err != nil {
		return classIncomplete
	}
	data, err := os.ReadFile(filepath.Join(dir, manifest.ManifestName))
	if err != nil || len(data) > 1<<20 {
		return classAnomaly
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return classAnomaly
	}
	if m.BackupID != filepath.Base(dir) || m.SchemaVersion != manifest.SchemaVersion {
		return classAnomaly
	}
	return classComplete
}

// DeleteBackup re-verifies the target, moves it into .trash with a unique
// name via atomic rename, then removes the trash directory and syncs.
func (s *Store) DeleteBackup(_ context.Context, b storage.Backup) error {
	dir := filepath.Join(s.backupsDir(), b.ID)
	if _, err := storage.ParseBackupID(b.ID); err != nil {
		return observability.NewSafe(observability.CodeRetentionFailed, "refusing to delete invalid backup id")
	}
	if !safefs.ContainsPath(s.backupsDir(), dir) {
		return observability.NewSafe(observability.CodeRetentionFailed, "backup path escapes backups dir")
	}
	if classifyComplete(dir) != classComplete {
		return observability.NewSafe(observability.CodeRetentionFailed,
			"backup lost its marker before deletion; skipped")
	}
	if err := os.Mkdir(s.trashBase(), 0o700); err != nil && !os.IsExist(err) {
		return observability.WrapSafe(observability.CodeRetentionFailed, "prepare trash dir", err)
	}
	trashDir := filepath.Join(s.trashBase(), fmt.Sprintf("%s-%d", b.ID, os.Getpid()))
	err := safefs.RenameNoReplace(dir, trashDir)
	if err == safefs.ErrExists {
		trashDir = filepath.Join(s.trashBase(), fmt.Sprintf("%s-%d-%s", b.ID, os.Getpid(), randomSuffix()))
		err = safefs.RenameNoReplace(dir, trashDir)
	}
	if err != nil {
		return observability.WrapSafe(observability.CodeRetentionFailed, "move backup to trash", err)
	}
	if err := os.RemoveAll(trashDir); err != nil {
		return observability.WrapSafe(observability.CodeRetentionFailed, "remove trashed backup", err)
	}
	var firstErr error
	for _, d := range []string{s.trashBase(), s.backupsDir()} {
		if err := safefs.SyncDir(d); err != nil && err != safefs.ErrDirSyncUnsupported && firstErr == nil {
			firstErr = observability.WrapSafe(observability.CodeRetentionFailed, "sync after deletion", err)
		}
	}
	return firstErr
}

// ListIncomplete collects stale staging dirs, trash dirs, and marker-less
// valid-ID backup dirs. Ages come from the parsed backup ID, never mtime.
func (s *Store) ListIncomplete(_ context.Context) ([]storage.IncompleteRun, error) {
	var out []storage.IncompleteRun
	collect := func(base string, idOf func(string) string, requireNoMarker bool) error {
		entries, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			id := idOf(e.Name())
			if id == "" {
				continue
			}
			startedAt, err := storage.ParseBackupID(id)
			if err != nil {
				continue
			}
			full := filepath.Join(base, e.Name())
			if requireNoMarker {
				if _, err := os.Lstat(filepath.Join(full, manifest.MarkerName)); err == nil {
					continue // published marker present: not incomplete
				}
			}
			out = append(out, storage.IncompleteRun{ID: id, StartedAt: startedAt, Location: full})
		}
		return nil
	}
	bareID := func(name string) string { return name }
	if err := collect(s.stagingBase(), stagingIDPart, false); err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed, "list staging", err)
	}
	if err := collect(s.trashBase(), stagingIDPart, false); err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed, "list trash", err)
	}
	// Backup directories are bare backup IDs, not <id>-<owner> names.
	if err := collect(s.backupsDir(), bareID, true); err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed, "list incomplete backups", err)
	}
	return out, nil
}

// stagingIDPart extracts the leading backup ID from a staging or trash name
// shaped <backup-id>-<owner>.
func stagingIDPart(name string) string {
	idx := strings.LastIndex(name, "-")
	if idx <= 0 {
		return ""
	}
	return name[:idx]
}

// DeleteIncomplete removes one incomplete run after re-validating that the
// location stays inside the managed roots.
func (s *Store) DeleteIncomplete(_ context.Context, r storage.IncompleteRun) error {
	if r.Location == "" {
		return observability.NewSafe(observability.CodeRetentionFailed, "incomplete run without location")
	}
	allowed := false
	for _, base := range []string{s.stagingBase(), s.trashBase(), s.backupsDir()} {
		if safefs.ContainsPath(base, r.Location) && r.Location != base {
			allowed = true
		}
	}
	if !allowed {
		return observability.NewSafe(observability.CodeRetentionFailed, "incomplete location outside managed roots")
	}
	if err := os.RemoveAll(r.Location); err != nil {
		return observability.WrapSafe(observability.CodeRetentionFailed, "remove incomplete run", err)
	}
	if err := safefs.SyncDir(filepath.Dir(r.Location)); err != nil && err != safefs.ErrDirSyncUnsupported {
		return observability.WrapSafe(observability.CodeRetentionFailed, "sync after incomplete cleanup", err)
	}
	return nil
}

// CleanupStaleUploads is a no-op for the local backend.
func (s *Store) CleanupStaleUploads(_ context.Context, _ time.Time) error {
	return nil
}
