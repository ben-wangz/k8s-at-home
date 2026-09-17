package s3

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

const (
	defaultPartSize int64 = 64 << 20
	maxParts              = 10000
	partTimeout           = 10 * time.Minute
)

// PutArchive uploads one finished archive conditionally and verifies it via HeadObject.
func (s *Store) PutArchive(ctx context.Context, a storage.Artifact) error {
	key := archiveObjectKey(s.prefix, s.identity.BackupID, manifest.ArchivePath(a.Name))
	f, err := os.Open(a.Path)
	if err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "open staged archive", err)
	}
	defer f.Close()
	if a.Size <= defaultPartSize {
		err = s.putSmallArchive(ctx, key, f, a)
	} else {
		err = s.uploadMultipart(ctx, key, f, a)
	}
	if err != nil {
		return err
	}
	return s.verifyArchive(ctx, key, a)
}

func (s *Store) putSmallArchive(ctx context.Context, key string, f *os.File, a storage.Artifact) error {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	in := &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           &key,
		Body:          f,
		ContentLength: int64Ptr(a.Size),
		IfNoneMatch:   strPtr("*"),
		Metadata:      map[string]string{"sha256": a.SHA256},
	}
	s.applySSE(in)
	_, err := s.client.PutObject(reqCtx, in)
	if err == nil {
		return nil
	}
	if isPreconditionFailed(err) || isResponseUncertain(err) {
		return s.reconcileArchive(ctx, key, a, err)
	}
	return observability.WrapSafe(observability.CodeStorageFailed, "archive put failed", err)
}

// reconcileArchive treats an exists-or-uncertain outcome as success only
// when the stored object matches our length and recorded digest metadata.
func (s *Store) reconcileArchive(ctx context.Context, key string, a storage.Artifact, cause error) error {
	head, err := s.headObject(ctx, key)
	if err != nil {
		return observability.WrapSafe(observability.CodePublishConflict,
			"archive key occupied and not verifiable", cause)
	}
	if head.ContentLength == nil || *head.ContentLength != a.Size || head.Metadata["sha256"] != a.SHA256 {
		return observability.WrapSafe(observability.CodePublishConflict,
			"archive key belongs to different content", cause)
	}
	return nil
}

func (s *Store) verifyArchive(ctx context.Context, key string, a storage.Artifact) error {
	head, err := s.headObject(ctx, key)
	if err != nil {
		return observability.WrapSafe(observability.CodeStorageFailed, "verify uploaded archive", err)
	}
	if head.ContentLength == nil || *head.ContentLength != a.Size {
		return observability.WrapSafe(observability.CodeStorageFailed, "uploaded archive length mismatch", nil)
	}
	if head.Metadata["sha256"] != a.SHA256 {
		return observability.WrapSafe(observability.CodeStorageFailed, "uploaded archive digest metadata mismatch", nil)
	}
	return nil
}

// uploadMultipart performs the explicit multipart protocol: create, upload
// parts sequentially from a SectionReader, conditionally complete. A 409 on
// completion aborts and restarts the whole upload once.
func (s *Store) uploadMultipart(ctx context.Context, key string, f *os.File, a storage.Artifact) error {
	partSize := choosePartSize(a.Size)
	for attempt := 0; attempt < 2; attempt++ {
		done, err := s.oneMultipartAttempt(ctx, key, f, a, partSize)
		if err == nil {
			return nil
		}
		if done || !isConflict(err) {
			return err
		}
		s.logger.Warn("multipart complete conflicted; restarting upload once",
			"errorCode", observability.CodeStorageFailed)
	}
	return observability.WrapSafe(observability.CodeStorageFailed,
		"multipart upload conflicted twice", nil)
}

// oneMultipartAttempt returns done=true when the error must not be retried.
func (s *Store) oneMultipartAttempt(ctx context.Context, key string, f *os.File, a storage.Artifact, partSize int64) (bool, error) {
	reqCtx, cancel := withTimeout(ctx)
	create, err := s.client.CreateMultipartUpload(reqCtx, &s3.CreateMultipartUploadInput{
		Bucket:   &s.bucket,
		Key:      &key,
		Metadata: map[string]string{"sha256": a.SHA256},
	})
	cancel()
	if err != nil {
		return true, observability.WrapSafe(observability.CodeStorageFailed, "create multipart upload", err)
	}
	uploadID := *create.UploadId
	s.trackUpload(key, uploadID)
	defer s.untrackUpload(key)

	parts, err := s.uploadParts(ctx, key, uploadID, f, a.Size, partSize)
	if err != nil {
		s.abortLogged(ctx, key, uploadID)
		return true, err
	}
	reqCtx, cancel = withTimeout(ctx)
	_, err = s.client.CompleteMultipartUpload(reqCtx, &s3.CompleteMultipartUploadInput{
		Bucket:          &s.bucket,
		Key:             &key,
		UploadId:        &uploadID,
		IfNoneMatch:     strPtr("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	cancel()
	if err == nil {
		return true, nil
	}
	if isPreconditionFailed(err) || isResponseUncertain(err) {
		// The completion may have been applied; trust only an exact match.
		if rerr := s.reconcileArchive(ctx, key, a, err); rerr == nil {
			s.abortLogged(ctx, key, uploadID)
			return true, nil
		}
	}
	if isConflict(err) || isNoSuchUpload(err) {
		s.abortLogged(ctx, key, uploadID)
		return false, observability.WrapSafe(observability.CodeStorageFailed,
			"multipart complete conflict", err)
	}
	s.abortLogged(ctx, key, uploadID)
	return true, observability.WrapSafe(observability.CodeStorageFailed, "complete multipart upload", err)
}

func (s *Store) uploadParts(ctx context.Context, key, uploadID string, f *os.File, size, partSize int64) ([]types.CompletedPart, error) {
	reader := io.NewSectionReader(f, 0, size)
	var parts []types.CompletedPart
	partNum := int32(1)
	for offset := int64(0); offset < size; offset += partSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		length := partSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		section := io.NewSectionReader(reader, offset, length)
		reqCtx, cancel := context.WithTimeout(ctx, partTimeout)
		out, err := s.client.UploadPart(reqCtx, &s3.UploadPartInput{
			Bucket:        &s.bucket,
			Key:           &key,
			UploadId:      &uploadID,
			PartNumber:    int32Ptr(partNum),
			Body:          section,
			ContentLength: int64Ptr(length),
		})
		cancel()
		if err != nil {
			return nil, observability.WrapSafe(observability.CodeStorageFailed, "upload part", err)
		}
		parts = append(parts, types.CompletedPart{
			PartNumber: int32Ptr(partNum),
			ETag:       out.ETag,
		})
		partNum++
	}
	return parts, nil
}

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
