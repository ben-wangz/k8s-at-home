package backends3

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type backendObjectLister interface {
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

func waitForDownloadCleanup(ctx context.Context, client backendObjectLister, bucket, prefix string) error {
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("completed S3 download retained temporary state objects: %w", err)
		}
		objects, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(1),
		})
		if err != nil {
			return fmt.Errorf("inspect completed S3 download cleanup: %w", err)
		}
		if len(objects.Contents) == 0 && !aws.ToBool(objects.IsTruncated) {
			return nil
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("completed S3 download retained temporary state objects: %w", ctx.Err())
		case <-timer.C:
		}
	}
}
