package s3

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"

	"git-repo-backup/internal/observability"
)

// CleanupStaleUploads aborts multipart uploads under the configured prefix
// initiated before the cutoff. This covers runs lost to SIGKILL or node
// failure, whose upload IDs could not be aborted by the process itself.
func (s *Store) CleanupStaleUploads(ctx context.Context, olderThan time.Time) error {
	pag := s3.NewListMultipartUploadsPaginator(s.client, &s3.ListMultipartUploadsInput{
		Bucket: &s.bucket,
		Prefix: strPtr(s.prefix + "/"),
	})
	for pag.HasMorePages() {
		page, err := pag.NextPage(ctx)
		if err != nil {
			return observability.WrapSafe(observability.CodeRetentionFailed,
				"list stale multipart uploads", err)
		}
		for _, up := range page.Uploads {
			if up.Key == nil || up.UploadId == nil || up.Initiated == nil {
				continue
			}
			if up.Initiated.After(olderThan) || up.Initiated.Equal(olderThan) {
				continue
			}
			if err := s.abortUpload(ctx, *up.Key, *up.UploadId); err != nil {
				return observability.WrapSafe(observability.CodeRetentionFailed,
					"abort stale multipart upload", err)
			}
			s.logger.Info("abandoned multipart upload aborted", "phase", "cleanup")
		}
	}
	return nil
}
