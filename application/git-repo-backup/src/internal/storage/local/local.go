// Package local implements the durable filesystem backend: staging under
// <root>/.staging, atomic publication with renameat2(RENAME_NOREPLACE),
// trash-based deletion, and non-blocking root locking.
package local

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/safefs"
	"git-repo-backup/internal/storage"
)

// Directory names inside the backup root.
const (
	dirBackups = "backups"
	dirStaging = ".staging"
	dirTrash   = ".trash"
	lockFile   = ".backup.lock"
)

// Store is both the RunStore and the Catalog for one local destination. A
// single non-blocking exclusive lock on <root>/.backup.lock covers writing,
// publication, and retention for the lifetime of the Store.
type Store struct {
	root    string
	lock    *safefs.Lock
	logger  *slog.Logger
	minFree uint64

	backupID   string
	stagingDir string
	published  bool
}

// New validates the destination, checks capacity and rename capability, and
// acquires the root lock.
func New(root string, minFree uint64, logger *slog.Logger) (*Store, error) {
	for _, d := range []string{"", dirBackups, dirStaging, dirTrash} {
		if err := safefs.EnsureDir(filepath.Join(root, d), 0o700); err != nil {
			return nil, observability.WrapSafe(observability.CodeStorageFailed,
				"prepare backup root directories", err)
		}
	}
	if err := safefs.CheckCapacity(root, minFree); err != nil {
		return nil, observability.WrapSafe(observability.CodeCapacity,
			"backup root capacity preflight", err)
	}
	if err := probeNoReplace(root); err != nil {
		return nil, err
	}
	lock, err := safefs.TryLock(filepath.Join(root, lockFile))
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed,
			"backup root lock unavailable (another run active?)", err)
	}
	return &Store{root: root, lock: lock, logger: logger, minFree: minFree}, nil
}

// probeNoReplace verifies the filesystem supports RENAME_NOREPLACE before
// anything is written, so publication cannot silently degrade.
func probeNoReplace(root string) error {
	base := filepath.Join(root, dirStaging, ".capability-probe")
	a, b := base+".a", base+".b"
	if err := os.Mkdir(a, 0o700); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "capability probe", err)
	}
	defer os.RemoveAll(a)
	defer os.RemoveAll(b)
	err := safefs.RenameNoReplace(a, b)
	if err == safefs.ErrNoReplaceUnsupported {
		return observability.WrapSafe(observability.CodeCapabilityMissing,
			"filesystem lacks atomic no-replace rename", err)
	}
	if err != nil && err != safefs.ErrExists {
		return observability.WrapSafe(observability.CodeStorageFailed, "capability probe", err)
	}
	return nil
}

func (s *Store) backupsDir() string  { return filepath.Join(s.root, dirBackups) }
func (s *Store) stagingBase() string { return filepath.Join(s.root, dirStaging) }
func (s *Store) trashBase() string   { return filepath.Join(s.root, dirTrash) }

// Begin verifies the backup ID is free and creates an exclusive staging
// directory named <backup-id>-<owner>.
func (s *Store) Begin(_ context.Context, id storage.RunIdentity) error {
	final := filepath.Join(s.backupsDir(), id.BackupID)
	if _, err := os.Lstat(final); err == nil {
		return observability.WrapSafe(observability.CodePublishConflict,
			"backup id already exists at destination", nil)
	}
	staging := filepath.Join(s.stagingBase(), id.BackupID+"-"+id.OwnerUUID)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed,
			"create staging directory", err)
	}
	if err := safefs.EnsureDir(filepath.Join(staging, manifest.RepositoriesDir), 0o700); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed,
			"create staging repositories directory", err)
	}
	s.backupID = id.BackupID
	s.stagingDir = staging
	s.published = false
	return nil
}

// ArchivePath returns the staging destination for a repository archive.
// Archives are written directly into staging; PutArchive only verifies them.
func (s *Store) ArchivePath(name string) string {
	return filepath.Join(s.stagingDir, manifest.RepositoriesDir, name+".tar.gz")
}

// PutArchive verifies a finished archive already sits in staging.
func (s *Store) PutArchive(_ context.Context, a storage.Artifact) error {
	if s.published || s.stagingDir == "" {
		return observability.WrapSafe(observability.CodeStorageFailed, "store not in staging state", nil)
	}
	info, err := os.Lstat(a.Path)
	if err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "archive missing in staging", err)
	}
	if !info.Mode().IsRegular() {
		return observability.WrapSafe(observability.CodeStorageFailed, "archive is not a regular file", nil)
	}
	if !safefs.ContainsPath(s.stagingDir, a.Path) {
		return observability.WrapSafe(observability.CodeStorageFailed, "archive outside staging", nil)
	}
	if info.Size() != a.Size {
		return observability.WrapSafe(observability.CodeStorageFailed, "archive size mismatch", nil)
	}
	return nil
}

// PutMetadata writes manifest.json and checksums.sha256 durably into staging.
func (s *Store) PutMetadata(_ context.Context, manifestBytes, checksumsBytes []byte) error {
	if s.published || s.stagingDir == "" {
		return observability.WrapSafe(observability.CodeStorageFailed, "store not in staging state", nil)
	}
	if err := safefs.WriteFileDurable(filepath.Join(s.stagingDir, manifest.ManifestName), manifestBytes, 0o600); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "write manifest", err)
	}
	if err := safefs.WriteFileDurable(filepath.Join(s.stagingDir, manifest.ChecksumsName), checksumsBytes, 0o600); err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "write checksums", err)
	}
	return nil
}
