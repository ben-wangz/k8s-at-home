// Package storage defines the backend-independent contracts shared by the
// runner and retention: the run store (Begin/Put/Commit/Abort) and the
// catalog (listing and deletion of complete and incomplete backups).
package storage

import (
	"context"
	"time"

	"git-repo-backup/internal/manifest"
)

// RunIdentity identifies one backup run for write-eligibility purposes.
type RunIdentity struct {
	BackupID  string
	OwnerUUID string
	StartedAt time.Time
}

// Artifact is one finished archive awaiting publication.
type Artifact struct {
	Name     string
	Path     string // staging path of the finished archive file
	SHA256   string
	Size     int64
	RefCount int
}

// PublishState describes how far a Commit progressed when it failed.
type PublishState int

const (
	// PublishNotPublished means no complete marker became durable.
	PublishNotPublished PublishState = iota
	// PublishPublished means the marker is committed and visible.
	PublishPublished
	// PublishPublishedDurabilityUncertain means the backup is visible but
	// a post-publication durability step (directory fsync) failed; the
	// backup must not be deleted or reported as rolled back.
	PublishPublishedDurabilityUncertain
	// PublishUnknown means the commit outcome could not be determined
	// (e.g. the marker response was lost and cannot be re-read). Data
	// that may be complete must not be deleted.
	PublishUnknown
)

// CommitError reports a commit failure together with the publication state
// reached before the failure.
type CommitError struct {
	State PublishState
	Err   error
}

func (e *CommitError) Error() string {
	return "commit failed after state " + e.State.String() + ": " + e.Err.Error()
}

func (e *CommitError) Unwrap() error { return e.Err }

func (s PublishState) String() string {
	switch s {
	case PublishPublished:
		return "published"
	case PublishPublishedDurabilityUncertain:
		return "published_durability_uncertain"
	case PublishUnknown:
		return "publication_unknown"
	default:
		return "not_published"
	}
}

// RunStore publishes exactly one run. Begin acquires the write eligibility
// for the backup ID (lock or claim); Commit is the one-way transition that
// makes the run visible; Abort removes unpublished data best-effort.
type RunStore interface {
	Begin(ctx context.Context, id RunIdentity) error
	// ArchivePath returns the local staging file the archive must be
	// written to before PutArchive is called.
	ArchivePath(name string) string
	PutArchive(ctx context.Context, a Artifact) error
	PutMetadata(ctx context.Context, manifestBytes, checksumsBytes []byte) error
	Commit(ctx context.Context, marker manifest.SuccessMarker) error
	Abort(ctx context.Context) error
	Close(ctx context.Context) error
}

// Backup is one recognized entry under the destination namespace.
type Backup struct {
	ID        string
	Complete  bool      // _SUCCESS present and small metadata valid
	Anomaly   bool      // valid ID but broken marker/metadata: never deleted, fails the job
	StartedAt time.Time // validated ID/claim time, not mtime
}

// IncompleteRun is leftover unpublished data eligible for age-based cleanup.
type IncompleteRun struct {
	ID        string
	StartedAt time.Time
	Location  string // backend-specific path or prefix
}

// Catalog lists and deletes backups and incomplete runs. Implementations
// paginate internally and never include unrelated names.
type Catalog interface {
	ListBackups(ctx context.Context) ([]Backup, error)
	DeleteBackup(ctx context.Context, b Backup) error
	ListIncomplete(ctx context.Context) ([]IncompleteRun, error)
	DeleteIncomplete(ctx context.Context, r IncompleteRun) error
	// CleanupStaleUploads aborts abandoned multipart uploads older than
	// the cutoff; local backends return nil.
	CleanupStaleUploads(ctx context.Context, olderThan time.Time) error
}
