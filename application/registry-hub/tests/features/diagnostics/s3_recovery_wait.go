package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func waitForStorageInventoryLeasesExpired(ctx context.Context, client *s3.Client, bucket string) error {
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	for {
		maintenance, err := readSeededLease(ctx, client, bucket, diagnosticsS3MaintenanceLease)
		if err != nil {
			return err
		}
		collector, err := readSeededLease(ctx, client, bucket, diagnosticsS3CollectorLease)
		if err != nil {
			return err
		}
		if !maintenance.active() && !collector.active() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("storage inventory leases did not expire: maintenance=%s collector=%s", maintenance.ExpiresAt.UTC().Format(time.RFC3339), collector.ExpiresAt.UTC().Format(time.RFC3339))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func readSeededLease(ctx context.Context, client *s3.Client, bucket, key string) (seededLeaseRecord, error) {
	data, err := readS3Object(ctx, client, bucket, key)
	if err != nil {
		return seededLeaseRecord{}, fmt.Errorf("read storage inventory lease %s: %w", key, err)
	}
	var lease seededLeaseRecord
	if err := json.Unmarshal(data, &lease); err != nil {
		return seededLeaseRecord{}, err
	}
	return lease, nil
}

func (lease seededLeaseRecord) active() bool {
	return lease.State == "held" && lease.ExpiresAt.After(time.Now())
}

func newSeededStorageCollection(snapshotID string) seededStorageCollection {
	return seededStorageCollection{
		Snapshot: seededStorageSnapshot{
			SnapshotID: snapshotID, StartedAt: time.Now().UTC(), UsageVersion: 1,
			Domains: map[string]map[string]any{
				"state":    {"domain": "state", "state": "collecting", "coverage": []string{}},
				"registry": {"domain": "registry", "state": "collecting", "coverage": []string{}},
			},
		},
		Usage: map[string]seededUsageRow{},
	}
}

func waitForSeededPendingBatch(ctx context.Context, client *s3.Client, bucket, expectedID string) (seededStorageCollection, error) {
	return pollSeededStorageCheckpoint(ctx, client, bucket, func(state seededStorageCollection) bool {
		return state.Snapshot.SnapshotID == expectedID && state.UsageRuns >= 1 && state.UsagePending == nil && state.AttributionPhase == 3
	})
}

func waitForLegacyStorageReplacement(ctx context.Context, client *s3.Client, bucket, oldID string) (seededStorageCollection, error) {
	return pollSeededStorageCheckpoint(ctx, client, bucket, func(state seededStorageCollection) bool {
		return state.Snapshot.SnapshotID != oldID && state.AutomaticRestarts == 1 && state.RecoveredFromBuildID == oldID
	})
}

func pollSeededStorageCheckpoint(ctx context.Context, client *s3.Client, bucket string, ready func(seededStorageCollection) bool) (seededStorageCollection, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		data, err := readS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"checkpoint.json")
		if err != nil {
			return seededStorageCollection{}, err
		}
		var state seededStorageCollection
		if err := json.Unmarshal(data, &state); err != nil {
			return seededStorageCollection{}, err
		}
		if ready(state) {
			return state, nil
		}
		select {
		case <-ctx.Done():
			return seededStorageCollection{}, ctx.Err()
		case <-deadline.C:
			return state, fmt.Errorf("seeded storage checkpoint did not recover: snapshot=%q phase=%d usageRuns=%d pending=%t restarts=%d recoveredFrom=%q", state.Snapshot.SnapshotID, state.AttributionPhase, state.UsageRuns, state.UsagePending != nil, state.AutomaticRestarts, state.RecoveredFromBuildID)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func putS3Object(ctx context.Context, client *s3.Client, bucket, key string, data []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data)))})
	return err
}

func readS3Object(ctx context.Context, client *s3.Client, bucket, key string) ([]byte, error) {
	response, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func readS3PublishedSnapshotID(ctx context.Context, client *s3.Client, bucket string) (string, error) {
	data, err := readS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"current.json")
	if err != nil {
		return "", err
	}
	var current struct {
		SnapshotID string `json:"snapshotID"`
	}
	if err := json.Unmarshal(data, &current); err != nil || current.SnapshotID == "" {
		return "", fmt.Errorf("published S3 summary pointer is invalid: %v", err)
	}
	return current.SnapshotID, nil
}

func verifyRetainedStorageSummary(ctx context.Context, environment testkit.CaseContext, expected report) error {
	var current report
	status, err := read(ctx, environment, "/api/v1/storage/summary?snapshotID="+expected.SnapshotID, &current)
	if err != nil || status != http.StatusOK || !reflect.DeepEqual(current, expected) {
		return fmt.Errorf("published S3 summary changed during recovery: status=%d err=%v", status, err)
	}
	return nil
}

func awaitNewStorageReport(ctx context.Context, environment testkit.CaseContext, expectedID string, started time.Time) (report, error) {
	deadline := time.NewTimer(15 * time.Minute)
	defer deadline.Stop()
	for {
		var snapshot report
		status, err := read(ctx, environment, "/api/v1/storage/summary", &snapshot)
		if err != nil {
			return report{}, err
		}
		if status == http.StatusOK && snapshot.SnapshotID == expectedID {
			if !snapshot.StartedAt.After(started) || snapshot.CompletedAt.IsZero() {
				return report{}, fmt.Errorf("replacement summary %q has invalid timestamps", expectedID)
			}
			if err := verifySummaryCoverage(snapshot); err != nil {
				return report{}, err
			}
			return snapshot, nil
		}
		if status != http.StatusOK && status != http.StatusServiceUnavailable {
			return report{}, fmt.Errorf("storage summary returned %d while waiting for replacement %q", status, expectedID)
		}
		select {
		case <-ctx.Done():
			return report{}, fmt.Errorf("replacement storage report %q was not published: %w", expectedID, ctx.Err())
		case <-deadline.C:
			return report{}, fmt.Errorf("replacement storage report %q was not published before deadline", expectedID)
		case <-time.After(2 * time.Second):
		}
	}
}

func scaleDiagnosticsBackend(ctx context.Context, environment testkit.CaseContext, replicas int) error {
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "scale", "deployment/"+environment.Fullname, fmt.Sprintf("--replicas=%d", replicas)); err != nil {
		return err
	}
	_, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=5m")
	return err
}
