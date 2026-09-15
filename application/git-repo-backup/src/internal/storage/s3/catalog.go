package s3

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"git-repo-backup/internal/manifest"
	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

// batchSize is the DeleteObjects per-request object limit.
const batchSize = 1000

// completeness mirrors the local backend's run classification.
type completeness uint8

const (
	classIncomplete completeness = iota
	classComplete
	classAnomaly
)

// listRunPrefixes returns the immediate child prefixes of <prefix>/, which
// are the run namespaces. Anything else (including .control) is skipped by
// the callers.
func (s *Store) listRunPrefixes(ctx context.Context) ([]string, error) {
	pag := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket:    &s.bucket,
		Prefix:    strPtr(s.prefix + "/"),
		Delimiter: strPtr("/"),
	})
	var prefixes []string
	for pag.HasMorePages() {
		page, err := pag.NextPage(ctx)
		if err != nil {
			return nil, observability.WrapSafe(observability.CodeStorageFailed, "list run prefixes", err)
		}
		for _, cp := range page.CommonPrefixes {
			if cp.Prefix != nil {
				prefixes = append(prefixes, *cp.Prefix)
			}
		}
	}
	return prefixes, nil
}

// runIDOf extracts the backup ID from a run prefix; empty when the prefix is
// not a valid ID namespace.
func runIDOf(prefix, full string) string {
	id := strings.TrimSuffix(strings.TrimPrefix(full, prefix+"/"), "/")
	if strings.Contains(id, "/") {
		return ""
	}
	return id
}

// ListBackups classifies run prefixes: complete backups need a marker and a
// structurally valid manifest; a marker with a broken manifest is an
// anomaly (never deleted, fails retention); no marker means incomplete.
func (s *Store) ListBackups(ctx context.Context) ([]storage.Backup, error) {
	prefixes, err := s.listRunPrefixes(ctx)
	if err != nil {
		return nil, err
	}
	var out []storage.Backup
	for _, rp := range prefixes {
		id := runIDOf(s.prefix, rp)
		startedAt, err := storage.ParseBackupID(id)
		if err != nil || id == controlSegment {
			continue // unrelated namespace; never considered, never deleted
		}
		b := storage.Backup{ID: id, StartedAt: startedAt}
		switch s.classifyRun(ctx, id) {
		case classComplete:
			b.Complete = true
		case classAnomaly:
			b.Anomaly = true
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Store) classifyRun(ctx context.Context, id string) completeness {
	if _, err := s.headObject(ctx, markerKey(s.prefix, id)); err != nil {
		return classIncomplete
	}
	data, err := s.getSmallObject(ctx, manifestKey(s.prefix, id))
	if err != nil {
		return classAnomaly
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return classAnomaly
	}
	if m.BackupID != id || m.SchemaVersion != manifest.SchemaVersion {
		return classAnomaly
	}
	return classComplete
}

// DeleteBackup removes the marker first, confirms it is gone, then batch-
// deletes the remaining objects of the run prefix.
func (s *Store) DeleteBackup(ctx context.Context, b storage.Backup) error {
	if _, err := storage.ParseBackupID(b.ID); err != nil {
		return observability.NewSafe(observability.CodeRetentionFailed, "refusing to delete invalid backup id")
	}
	if _, err := s.headObject(ctx, markerKey(s.prefix, b.ID)); err != nil {
		return observability.NewSafe(observability.CodeRetentionFailed,
			"backup lost its marker before deletion; skipped")
	}
	if err := s.deleteObject(ctx, markerKey(s.prefix, b.ID)); err != nil {
		return err
	}
	if _, err := s.headObject(ctx, markerKey(s.prefix, b.ID)); !isNotFound(err) {
		return observability.NewSafe(observability.CodeRetentionFailed,
			"marker still present after deletion; skipping rest")
	}
	return s.deletePrefixObjects(ctx, runPrefix(s.prefix, b.ID))
}

func (s *Store) deleteObject(ctx context.Context, key string) error {
	reqCtx, cancel := withTimeout(ctx)
	defer cancel()
	_, err := s.client.DeleteObject(reqCtx, &s3.DeleteObjectInput{
		Bucket: &s.bucket, Key: &key,
	})
	if err != nil {
		return observability.WrapSafe(observability.CodeRetentionFailed, "delete object", err)
	}
	return nil
}

// deletePrefixObjects batch-deletes every object under prefix, checking the
// per-object errors inside successful HTTP responses.
func (s *Store) deletePrefixObjects(ctx context.Context, prefix string) error {
	pag := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket,
		Prefix: &prefix,
	})
	var batch []types.ObjectIdentifier
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		reqCtx, cancel := withTimeout(ctx)
		defer cancel()
		out, err := s.client.DeleteObjects(reqCtx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket,
			Delete: &types.Delete{Objects: batch},
		})
		batch = batch[:0]
		if err != nil {
			return observability.WrapSafe(observability.CodeRetentionFailed, "batch delete", err)
		}
		if len(out.Errors) > 0 {
			return observability.NewSafe(observability.CodeRetentionFailed,
				"batch delete reported per-object errors")
		}
		return nil
	}
	for pag.HasMorePages() {
		page, err := pag.NextPage(ctx)
		if err != nil {
			return observability.WrapSafe(observability.CodeRetentionFailed, "list objects for deletion", err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			batch = append(batch, types.ObjectIdentifier{Key: obj.Key})
			if len(batch) >= batchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}
