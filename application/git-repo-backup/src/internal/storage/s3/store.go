package s3

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

// requestTimeout bounds each individual S3 API call; every call is also
// bounded by the run context.
const requestTimeout = 2 * time.Minute

// errExists reports a conditional write rejected because the object exists.
var errExists = errors.New("object already exists")

// Store implements storage.RunStore and storage.Catalog against one bucket
// and prefix. Write eligibility is the conditional-write claim object.
type Store struct {
	client     *s3.Client
	bucket     string
	prefix     string
	cfg        config.S3Storage
	logger     *slog.Logger
	stagingDir string

	identity    storage.RunIdentity
	activeMu    sync.Mutex
	activeParts map[string]string // object key -> upload ID
}

// New builds the store and its client.
func New(ctx context.Context, cfg config.S3Storage, logger *slog.Logger) (*Store, error) {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{
		client:      client,
		bucket:      cfg.Bucket,
		prefix:      cfg.Prefix,
		cfg:         cfg,
		logger:      logger,
		activeParts: make(map[string]string),
	}, nil
}

// claimBody is the JSON stored in the claim object. It is intentionally
// non-sensitive: no URLs, no credentials.
type claimBody struct {
	BackupID  string    `json:"backupId"`
	Owner     string    `json:"owner"`
	StartedAt time.Time `json:"startedAt"`
}

// Begin reserves the backup ID: the final prefix must be empty, the claim
// must be created conditionally, and the final prefix must still be empty
// afterwards. An uncertain claim response fails closed because ownership
// cannot be safely established for subsequent writes.
func (s *Store) Begin(ctx context.Context, id storage.RunIdentity) error {
	final := runPrefix(s.prefix, id.BackupID)
	empty, err := s.prefixIsEmpty(ctx, final)
	if err != nil {
		return err
	}
	if !empty {
		return observability.WrapSafe(observability.CodePublishConflict,
			"final backup prefix already has objects", nil)
	}
	body, err := json.Marshal(claimBody{BackupID: id.BackupID, Owner: id.OwnerUUID, StartedAt: id.StartedAt})
	if err != nil {
		return err
	}
	ck := claimKey(s.prefix, id.BackupID)
	if err := s.putIfAbsent(ctx, ck, body, "application/json"); err != nil {
		if errors.Is(err, errExists) {
			return observability.WrapSafe(observability.CodePublishConflict,
				"backup id claim already exists", err)
		}
		if isResponseUncertain(err) {
			return observability.WrapSafe(observability.CodePublishUnknown,
				"claim creation outcome is unknown; refusing to continue", err)
		}
		return err
	}
	empty, err = s.prefixIsEmpty(ctx, final)
	if err != nil {
		return err
	}
	if !empty {
		return observability.WrapSafe(observability.CodePublishConflict,
			"data appeared in final prefix during begin; leaving it untouched", nil)
	}
	s.identity = id
	return nil
}

// ArchivePath returns the local staging file for an archive. The S3 backend
// stages archives in the writable workspace, never inside the mirror tree.
func (s *Store) ArchivePath(name string) string {
	return s.stagingDir + "/" + name + ".tar.gz"
}

// SetStagingDir configures where archives are staged locally before upload.
func (s *Store) SetStagingDir(dir string) { s.stagingDir = dir }

// Abort aborts every tracked multipart upload. Already-uploaded objects stay
// behind as incomplete data for later cleanup; the marker is never written.
func (s *Store) Abort(ctx context.Context) error {
	s.activeMu.Lock()
	pending := make(map[string]string, len(s.activeParts))
	for k, v := range s.activeParts {
		pending[k] = v
	}
	s.activeParts = make(map[string]string)
	s.activeMu.Unlock()
	var firstErr error
	for key, uploadID := range pending {
		if err := s.abortUpload(ctx, key, uploadID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close releases client resources; the SDK client needs no teardown.
func (s *Store) Close(context.Context) error { return nil }

// statusOf extracts the HTTP status from a smithy response error.
func statusOf(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

// isResponseUncertain reports errors where the request may have been applied
// despite the error: network failures, timeouts, and connection resets. A
// definitive HTTP response means the service processed the request.
func isResponseUncertain(err error) bool {
	if err == nil {
		return false
	}
	return statusOf(err) == 0
}

// isPreconditionFailed matches 412 responses on conditional writes. The S3
// model has no dedicated PreconditionFailed typed error, so the smithy
// error code is inspected directly.
func isPreconditionFailed(err error) bool {
	var ae smithy.APIError
	if errors.As(err, &ae) && ae.ErrorCode() == "PreconditionFailed" {
		return true
	}
	return statusOf(err) == http.StatusPreconditionFailed
}

// isNotFound matches 404 responses.
func isNotFound(err error) bool {
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	return statusOf(err) == http.StatusNotFound
}

// isConflict matches 409 responses (e.g. multipart completion races).
func isConflict(err error) bool { return statusOf(err) == http.StatusConflict }

// withTimeout applies the per-request timeout to the run context.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, requestTimeout)
}
