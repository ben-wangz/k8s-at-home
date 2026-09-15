package s3

import (
	"context"
	"encoding/json"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"git-repo-backup/internal/observability"
	"git-repo-backup/internal/storage"
)

// ListIncomplete finds run prefixes without a marker. Age comes from the
// run's claim startedAt, falling back to the oldest object LastModified.
func (s *Store) ListIncomplete(ctx context.Context) ([]storage.IncompleteRun, error) {
	prefixes, err := s.listRunPrefixes(ctx)
	if err != nil {
		return nil, err
	}
	var out []storage.IncompleteRun
	for _, rp := range prefixes {
		id := runIDOf(s.prefix, rp)
		if id == "" || id == controlSegment {
			continue
		}
		if _, err := storage.ParseBackupID(id); err != nil {
			continue
		}
		if _, err := s.headObject(ctx, markerKey(s.prefix, id)); err == nil {
			continue // complete marker present
		}
		startedAt, ok := s.runStartTime(ctx, id, rp)
		if !ok {
			continue
		}
		out = append(out, storage.IncompleteRun{ID: id, StartedAt: startedAt, Location: rp})
	}
	return out, nil
}

// runStartTime prefers the claim's recorded start time and falls back to the
// oldest object timestamp when no claim can be read.
func (s *Store) runStartTime(ctx context.Context, id, prefix string) (time.Time, bool) {
	data, err := s.getSmallObject(ctx, claimKey(s.prefix, id))
	if err == nil {
		var c claimBody
		if json.Unmarshal(data, &c) == nil && c.BackupID == id && !c.StartedAt.IsZero() {
			return c.StartedAt, true
		}
	}
	oldest, ok := s.oldestObjectTime(ctx, prefix)
	if !ok {
		return time.Time{}, false
	}
	return oldest, true
}

func (s *Store) oldestObjectTime(ctx context.Context, prefix string) (time.Time, bool) {
	pag := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: &s.bucket, Prefix: &prefix,
	})
	var oldest time.Time
	for pag.HasMorePages() {
		page, err := pag.NextPage(ctx)
		if err != nil {
			return time.Time{}, false
		}
		for _, obj := range page.Contents {
			if obj.LastModified == nil {
				continue
			}
			if oldest.IsZero() || obj.LastModified.Before(oldest) {
				oldest = *obj.LastModified
			}
		}
	}
	return oldest, !oldest.IsZero()
}

// DeleteIncomplete removes every object of an incomplete run prefix. The
// permanent claim is intentionally kept.
func (s *Store) DeleteIncomplete(ctx context.Context, r storage.IncompleteRun) error {
	id := runIDOf(s.prefix, r.Location)
	if _, err := storage.ParseBackupID(id); err != nil {
		return observability.NewSafe(observability.CodeRetentionFailed, "refusing invalid incomplete prefix")
	}
	want := runPrefix(s.prefix, id)
	if r.Location != want {
		return observability.NewSafe(observability.CodeRetentionFailed, "incomplete prefix mismatch")
	}
	return s.deletePrefixObjects(ctx, r.Location)
}
