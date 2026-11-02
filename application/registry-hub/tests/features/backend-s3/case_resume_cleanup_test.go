package backends3

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type cleanupObjectLister func(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error)

func (list cleanupObjectLister) ListObjectsV2(ctx context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return list(ctx, input)
}

func TestCompletedDownloadWaitsForAsynchronousCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	client := cleanupObjectLister(func(_ context.Context, input *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
		calls++
		if aws.ToString(input.Bucket) != "bucket" || aws.ToString(input.Prefix) != "downloads/session/" || aws.ToInt32(input.MaxKeys) != 1 {
			t.Fatalf("cleanup polling escaped its bounded session prefix: %+v", input)
		}
		if calls == 1 {
			return &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("downloads/session/state.json")}}}, nil
		}
		return &s3.ListObjectsV2Output{}, nil
	})
	if err := waitForDownloadCleanup(ctx, client, "bucket", "downloads/session/"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("cleanup was not observed: calls=%d", calls)
	}
}

func TestDownloadCleanupRejectsStorageFailureAndUnfinishedCleanup(t *testing.T) {
	t.Run("storage failure", func(t *testing.T) {
		failure := errors.New("S3 unavailable")
		client := cleanupObjectLister(func(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			return nil, failure
		})
		if err := waitForDownloadCleanup(context.Background(), client, "bucket", "session/"); !errors.Is(err, failure) {
			t.Fatalf("storage failure was hidden: %v", err)
		}
	})
	t.Run("unfinished cleanup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client := cleanupObjectLister(func(context.Context, *s3.ListObjectsV2Input) (*s3.ListObjectsV2Output, error) {
			cancel()
			return &s3.ListObjectsV2Output{IsTruncated: aws.Bool(true)}, nil
		})
		if err := waitForDownloadCleanup(ctx, client, "bucket", "session/"); !errors.Is(err, context.Canceled) {
			t.Fatalf("unfinished cleanup was accepted: %v", err)
		}
	})
}
