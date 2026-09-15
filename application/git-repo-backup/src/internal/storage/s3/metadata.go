package s3

import (
	"context"
	"errors"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

// PutMetadata uploads manifest.json and then checksums.sha256, both with
// conditional writes, after all archives succeeded.
func (s *Store) PutMetadata(ctx context.Context, manifestBytes, checksumsBytes []byte) error {
	mk := manifestKey(s.prefix, s.identity.BackupID)
	if err := s.putVerifiedSmall(ctx, mk, manifestBytes, "application/json"); err != nil {
		return err
	}
	ck := runObjectKey(s.prefix, s.identity.BackupID, manifest.ChecksumsName)
	return s.putVerifiedSmall(ctx, ck, checksumsBytes, "text/plain")
}

// putVerifiedSmall conditionally writes a small object. On an exists or
// response-uncertain outcome it re-reads the object and continues only on a
// byte-identical match.
func (s *Store) putVerifiedSmall(ctx context.Context, key string, body []byte, contentType string) error {
	err := s.putIfAbsent(ctx, key, body, contentType)
	if err == nil {
		return nil
	}
	if !errors.Is(err, errExists) && !isResponseUncertain(err) {
		return err
	}
	got, gerr := s.getSmallObject(ctx, key)
	if gerr != nil || string(got) != string(body) {
		return observability.WrapSafe(observability.CodePublishConflict,
			"object key occupied by different content", err)
	}
	return nil
}

// Commit writes the _SUCCESS marker last with a conditional put. When the
// marker response is lost, the stored bytes decide: an exact match means the
// run committed; anything else is reported as publication_unknown.
func (s *Store) Commit(ctx context.Context, marker manifest.SuccessMarker) error {
	data, err := marker.Encode()
	if err != nil {
		return &storage.CommitError{State: storage.PublishNotPublished, Err: err}
	}
	key := markerKey(s.prefix, s.identity.BackupID)
	err = s.putIfAbsent(ctx, key, data, "application/json")
	if err == nil {
		return nil
	}
	if !errors.Is(err, errExists) && !isResponseUncertain(err) {
		return &storage.CommitError{State: storage.PublishNotPublished,
			Err: observability.WrapSafe(observability.CodeStorageFailed, "marker upload failed", err)}
	}
	got, gerr := s.getSmallObject(ctx, key)
	if gerr == nil && string(got) == string(data) {
		return nil // the lost response had actually applied
	}
	state := storage.PublishNotPublished
	code := observability.CodePublishConflict
	if gerr != nil {
		state = storage.PublishUnknown
		code = observability.CodePublishUnknown
	}
	return &storage.CommitError{State: state,
		Err: observability.WrapSafe(code, "cannot confirm marker state", err)}
}
