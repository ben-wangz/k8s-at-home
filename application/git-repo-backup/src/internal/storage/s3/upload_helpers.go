package s3

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"git-repo-backup/internal/observability"
)

// choosePartSize keeps the part count within the 10000-part service limit.
func choosePartSize(size int64) int64 {
	partSize := defaultPartSize
	for size/partSize > maxParts {
		partSize *= 2
	}
	return partSize
}

func (s *Store) abortLogged(ctx context.Context, key, uploadID string) {
	if err := s.abortUpload(ctx, key, uploadID); err != nil {
		s.logger.Warn("multipart abort failed", "errorCode", observability.CodeStorageFailed)
	}
}

func (s *Store) abortUpload(ctx context.Context, key, uploadID string) error {
	// A cancelled run context is expected during SIGTERM cleanup. Keep the
	// abort request alive long enough to reach S3; normal cleanup contexts still
	// retain their caller deadline and cancellation semantics.
	if ctx.Err() != nil {
		ctx = context.WithoutCancel(ctx)
	}
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.client.AbortMultipartUpload(reqCtx, &s3.AbortMultipartUploadInput{
		Bucket:   &s.bucket,
		Key:      &key,
		UploadId: &uploadID,
	})
	if err != nil && isNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) trackUpload(key, uploadID string) {
	s.activeMu.Lock()
	s.activeParts[key] = uploadID
	s.activeMu.Unlock()
}

func (s *Store) untrackUpload(key string) {
	s.activeMu.Lock()
	delete(s.activeParts, key)
	s.activeMu.Unlock()
}

// isNoSuchUpload detects uploads aborted or expired remotely.
func isNoSuchUpload(err error) bool {
	var n *types.NoSuchUpload
	return errors.As(err, &n)
}
