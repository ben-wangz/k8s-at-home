package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func measureReceiptCleanupDueRetry(ctx context.Context, client *s3.Client, bucket, runID string) (map[string]any, error) {
	before, err := readS3ScanCheckpoint(ctx, client, bucket, "source-checkpoints/receipt_cleanup.json")
	if err != nil {
		return nil, fmt.Errorf("read receipt cleanup retry baseline: %w", err)
	}
	started := time.Now().UTC()
	receiptPath := "background/v1/receipts/registry-hub-test/" + runID + ".json"
	receiptObject := "registry-hub-backend/" + receiptPath
	retryObject := "registry-hub-backend/background/v1/source-retries/receipt_cleanup/"
	duePrefix := "registry-hub-backend/background/v1/source-retry-due/receipt_cleanup/"
	defer func() {
		_, _ = client.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(receiptObject)})
	}()
	if err := putS3Object(ctx, client, bucket, receiptObject, []byte(`{}`)); err != nil {
		return nil, err
	}
	source, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(receiptObject)})
	if err != nil || aws.ToString(source.ETag) == "" {
		return nil, fmt.Errorf("read seeded receipt version: %v", err)
	}
	retryID, err := seededSourceSweepRetryID("receipt_cleanup", "default", receiptPath, aws.ToString(source.ETag))
	if err != nil {
		return nil, err
	}
	retryKey := retryObject + retryID + ".json"
	dueAt := started.Add(-15 * time.Second)
	dueKey := duePrefix + seededSourceSweepRetryDueSuffix(dueAt, retryID, 1)
	defer func() {
		_, _ = client.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(retryKey)})
		_, _ = client.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(dueKey)})
	}()
	retry := map[string]any{
		"schemaVersion": 1, "id": retryID, "kind": "receipt_cleanup", "partition": "default",
		"sourceKey": receiptPath, "sourceVersion": aws.ToString(source.ETag), "revision": 1, "attempts": 1,
		"state": "pending", "errorClass": "fixture_due_retry", "nextAttemptAt": dueAt,
		"createdAt": started.Add(-time.Minute), "updatedAt": started,
	}
	due := map[string]any{"retryID": retryID, "revision": 1, "dueAt": dueAt}
	if err := putS3JSON(ctx, client, bucket, retryKey, retry); err != nil {
		return nil, err
	}
	if err := putS3JSON(ctx, client, bucket, dueKey, due); err != nil {
		return nil, err
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		status, readErr := readS3ScanCheckpoint(ctx, client, bucket, "source-checkpoints/receipt_cleanup.json")
		if readErr != nil {
			return nil, readErr
		}
		retryPresent, presenceErr := s3ObjectExists(ctx, client, bucket, retryKey)
		if presenceErr != nil {
			return nil, presenceErr
		}
		duePresent, presenceErr := s3ObjectExists(ctx, client, bucket, dueKey)
		if presenceErr != nil {
			return nil, presenceErr
		}
		if !retryPresent && !duePresent && status.Operations > before.Operations && status.DispatchLagSeconds >= 10 {
			observedAt := time.Now().UTC()
			return map[string]any{
				"kind": "receipt_cleanup", "attempts": 1, "scheduledAt": dueAt,
				"observedAt": observedAt, "dispatchLagSeconds": status.DispatchLagSeconds,
				"observedDispatchLatencySeconds": observedAt.Sub(dueAt).Seconds(),
				"elapsedSeconds":                 observedAt.Sub(started).Seconds(), "operationsBefore": before.Operations,
				"operationsAfter": status.Operations, "pendingRetriesBefore": before.PendingRetries,
				"pendingRetriesAfter": status.PendingRetries, "retryRecordRemoved": true,
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("receipt cleanup due retry did not complete: retryPresent=%t duePresent=%t lag=%.3f pending=%d operations=%d baselineOperations=%d lastHeartbeat=%s lastCursorProgress=%s nextAttemptAt=%s lastScanCompletedAt=%s", retryPresent, duePresent, status.DispatchLagSeconds, status.PendingRetries, status.Operations, before.Operations, status.LastHeartbeatAt.UTC().Format(time.RFC3339Nano), status.LastCursorProgressAt.UTC().Format(time.RFC3339Nano), status.NextAttemptAt.UTC().Format(time.RFC3339Nano), status.LastScanCompletedAt.UTC().Format(time.RFC3339Nano))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func readS3ScanCheckpoint(ctx context.Context, client *s3.Client, bucket, path string) (scanStatusEvidence, error) {
	data, err := readS3Object(ctx, client, bucket, "registry-hub-backend/background/v1/"+path)
	if err != nil {
		return scanStatusEvidence{}, err
	}
	var checkpoint scanCheckpointEvidence
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return scanStatusEvidence{}, err
	}
	return checkpoint.Status, nil
}

func waitForStorageInventoryScan(ctx context.Context, client *s3.Client, bucket string, started time.Time) (scanStatusEvidence, error) {
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	for {
		status, err := readS3ScanCheckpoint(ctx, client, bucket, "maintenance-checkpoints/storage_inventory.json")
		if err != nil {
			return scanStatusEvidence{}, err
		}
		if status.Complete && status.LastScanCompletedAt.After(started) {
			return status, nil
		}
		select {
		case <-ctx.Done():
			return scanStatusEvidence{}, ctx.Err()
		case <-deadline.C:
			return status, fmt.Errorf("storage inventory scan did not complete after %s: complete=%t lastCompleted=%s", started.UTC().Format(time.RFC3339Nano), status.Complete, status.LastScanCompletedAt.UTC().Format(time.RFC3339Nano))
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func seededSourceSweepRetryID(kind, partition, sourceKey, sourceVersion string) (string, error) {
	data, err := json.Marshal([]string{kind, partition, sourceKey, sourceVersion})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func seededSourceSweepRetryDueSuffix(dueAt time.Time, retryID string, revision uint64) string {
	return dueAt.UTC().Format("20060102T150405.000000000Z") + "/" + retryID + fmt.Sprintf("-%020d.json", revision)
}

func putS3JSON(ctx context.Context, client *s3.Client, bucket, key string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return putS3Object(ctx, client, bucket, key, data)
}

func s3ObjectExists(ctx context.Context, client *s3.Client, bucket, key string) (bool, error) {
	result, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(key), MaxKeys: aws.Int32(1)})
	if err != nil {
		return false, err
	}
	for _, object := range result.Contents {
		if aws.ToString(object.Key) == key {
			return true, nil
		}
	}
	return false, nil
}
