package local

import (
	"context"
	"os"
	"path/filepath"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
	"git-repo-backup/internal/storage"
)

// Commit writes _SUCCESS exclusively, syncs the staging directory, renames
// staging to backups/<id> with RENAME_NOREPLACE, and syncs both parents.
func (s *Store) Commit(_ context.Context, marker manifest.SuccessMarker) error {
	if s.stagingDir == "" || s.published {
		return &storage.CommitError{State: storage.PublishNotPublished,
			Err: observability.NewSafe(observability.CodeStorageFailed, "store not in staging state")}
	}
	data, err := marker.Encode()
	if err != nil {
		return &storage.CommitError{State: storage.PublishNotPublished,
			Err: observability.WrapSafe(observability.CodeStorageFailed, "encode marker", err)}
	}
	if err := safefs.WriteFileDurable(filepath.Join(s.stagingDir, manifest.MarkerName), data, 0o600); err != nil {
		return &storage.CommitError{State: storage.PublishNotPublished,
			Err: observability.WrapSafe(observability.CodeStorageFailed, "write success marker", err)}
	}
	s.warnSyncUnsupported(safefs.SyncDir(s.stagingDir), "staging directory")
	final := filepath.Join(s.backupsDir(), s.backupID)
	if err := safefs.RenameNoReplace(s.stagingDir, final); err != nil {
		return &storage.CommitError{State: storage.PublishNotPublished, Err: renameError(err)}
	}
	s.published = true
	s.stagingDir = ""
	if err := s.syncParents(); err != nil {
		return &storage.CommitError{State: storage.PublishPublishedDurabilityUncertain, Err: err}
	}
	return nil
}

func renameError(err error) error {
	switch err {
	case safefs.ErrExists:
		return observability.WrapSafe(observability.CodePublishConflict,
			"backup id already exists at destination", nil)
	case safefs.ErrNoReplaceUnsupported:
		return observability.WrapSafe(observability.CodeCapabilityMissing,
			"filesystem lacks atomic no-replace rename", err)
	}
	return observability.WrapSafe(observability.CodeStorageFailed, "publish staging directory", err)
}

// syncParents fsyncs .staging and backups after the rename. Real I/O errors
// after a successful rename leave a visible backup with uncertain durability;
// capability gaps are only warned about.
func (s *Store) syncParents() error {
	var firstErr error
	for _, d := range []string{s.stagingBase(), s.backupsDir()} {
		if err := safefs.SyncDir(d); err != nil {
			if err == safefs.ErrDirSyncUnsupported {
				s.warnSyncUnsupported(err, "parent directory")
				continue
			}
			if firstErr == nil {
				firstErr = observability.WrapSafe(observability.CodeStorageFailed,
					"sync parent directory after publish", err)
			}
		}
	}
	return firstErr
}

// Abort removes unpublished staging data. A committed backup is never
// deleted here, even when a later step failed.
func (s *Store) Abort(_ context.Context) error {
	if s.published || s.stagingDir == "" {
		return nil
	}
	if err := os.RemoveAll(s.stagingDir); err != nil {
		s.logger.Warn("abort could not remove staging", "errorCode", observability.CodeStorageFailed)
		return observability.WrapSafe(observability.CodeStorageFailed, "remove staging during abort", err)
	}
	staging := s.stagingDir
	s.stagingDir = ""
	s.warnSyncUnsupported(safefs.SyncDir(filepath.Dir(staging)), "staging parent")
	return nil
}

// Close releases the root lock.
func (s *Store) Close(_ context.Context) error { return s.lock.Close() }

func (s *Store) warnSyncUnsupported(err error, what string) {
	if err == nil {
		return
	}
	if err == safefs.ErrDirSyncUnsupported {
		s.logger.Warn("directory fsync unsupported; power-loss durability depends on the volume",
			"target", what, "errorCode", observability.CodeCapabilityMissing)
	}
}
