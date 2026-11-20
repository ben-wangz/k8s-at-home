package diagnostics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const (
	diagnosticsS3SecretSuffix     = "-external-backend-s3"
	diagnosticsS3ObjectPrefix     = "registry-hub-backend/diagnostics/v1/storage/"
	diagnosticsStorageKeyPrefix   = "diagnostics/v1/storage/"
	diagnosticsS3Maintenance      = "registry-hub-backend/background/v1/maintenance-checkpoints/storage_inventory.json"
	diagnosticsS3MaintenanceLease = "registry-hub-backend/leases/background/maintenance/storage_inventory"
	diagnosticsS3CollectorLease   = "registry-hub-backend/leases/diagnostics/storage-inventory"
)

type seededUsageRow struct {
	Mask                    uint8  `json:"groupMask,omitempty"`
	Category                string `json:"category,omitempty"`
	Repository              string `json:"repository,omitempty"`
	ImageID                 string `json:"imageID,omitempty"`
	Platform                string `json:"platform,omitempty"`
	Domain                  string `json:"domain"`
	Dimension               string `json:"dimension"`
	Name                    string `json:"name"`
	ObjectBytes             int64  `json:"objectBytes"`
	Objects                 int64  `json:"objects"`
	LogicalBytes            *int64 `json:"logicalBytes"`
	ResidentPayloadBytes    *int64 `json:"residentPayloadBytes"`
	LogicalUnknown          bool   `json:"logicalUnknown"`
	ExclusiveBytes          *int64 `json:"exclusiveBytes"`
	SharedBytes             *int64 `json:"sharedBytes"`
	Attribution             string `json:"attribution"`
	UnknownAttributionBytes int64  `json:"unknownAttributionBytes"`
}

type seededUsageRun struct {
	Stage string `json:"stage"`
	Level int    `json:"level"`
	Index int    `json:"index"`
}

type seededUsageManifest struct {
	Pages int `json:"pages"`
	Rows  int `json:"rows"`
}

type seededCursor struct {
	Done bool `json:"done,omitempty"`
}

type seededPendingBatch struct {
	Version        int                 `json:"version"`
	BatchID        string              `json:"batchID"`
	SnapshotID     string              `json:"snapshotID"`
	Run            seededUsageRun      `json:"run"`
	StartCursor    seededCursor        `json:"startCursor"`
	NextCursor     seededCursor        `json:"nextCursor"`
	NextPhase      int                 `json:"nextPhase"`
	PayloadKey     string              `json:"payloadKey"`
	PayloadSHA256  string              `json:"payloadSHA256"`
	PageKey        string              `json:"pageKey"`
	PageSHA256     string              `json:"pageSHA256"`
	ManifestKey    string              `json:"manifestKey"`
	Manifest       seededUsageManifest `json:"manifest"`
	ManifestSHA256 string              `json:"manifestSHA256"`
	PreparedAt     time.Time           `json:"preparedAt"`
}

type seededStorageSnapshot struct {
	SnapshotID   string                    `json:"snapshotID"`
	StartedAt    time.Time                 `json:"startedAt"`
	UsageVersion int                       `json:"usageVersion"`
	Domains      map[string]map[string]any `json:"domains"`
}

type seededStorageCollection struct {
	Snapshot               seededStorageSnapshot     `json:"snapshot"`
	Domain                 int                       `json:"domain"`
	UsageRuns              int                       `json:"usageRuns"`
	Usage                  map[string]seededUsageRow `json:"usage"`
	AttributionPhase       int                       `json:"attributionPhase"`
	AttributionUsageCursor seededCursor              `json:"attributionUsageCursor"`
	UsagePending           *seededPendingBatch       `json:"usagePending,omitempty"`
	AutomaticRestarts      int                       `json:"automaticRestarts,omitempty"`
	RecoveredFromBuildID   string                    `json:"recoveredFromBuildID,omitempty"`
}

type seededMaintenanceCheckpoint struct {
	Next time.Time `json:"next"`
}

type seededLeaseRecord struct {
	State     string    `json:"state"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type scanStatusEvidence struct {
	StartedAt            time.Time        `json:"startedAt"`
	ObservedAt           time.Time        `json:"observedAt"`
	ProgressAt           time.Time        `json:"progressAt"`
	LastScanCompletedAt  time.Time        `json:"lastScanCompletedAt"`
	LastSuccessAt        time.Time        `json:"lastSuccessAt"`
	LastEntityProgressAt time.Time        `json:"lastEntityProgressAt"`
	LastCursorProgressAt time.Time        `json:"lastCursorProgressAt"`
	LastHeartbeatAt      time.Time        `json:"lastHeartbeatAt"`
	LastSuccessfulRound  uint64           `json:"lastSuccessfulRound"`
	OldestDueRetryAt     time.Time        `json:"oldestDueRetryAt,omitempty"`
	NextRetryAt          time.Time        `json:"nextRetryAt,omitempty"`
	NextAttemptAt        time.Time        `json:"nextAttemptAt,omitempty"`
	DispatchLagSeconds   float64          `json:"dispatchLagSeconds"`
	PendingRetries       int64            `json:"pendingRetries"`
	Operations           uint64           `json:"operations"`
	Processed            uint64           `json:"processed"`
	Counts               map[string]int64 `json:"counts,omitempty"`
	CheckpointAt         time.Time        `json:"checkpointAt,omitempty"`
	Complete             bool             `json:"complete"`
}

type scanCheckpointEvidence struct {
	Status scanStatusEvidence `json:"status"`
}

func verifySeededStorageRecoveryS3(ctx context.Context, environment testkit.CaseContext, published report) (map[string]any, error) {
	client, bucket, err := diagnosticsS3Client(ctx, environment)
	if err != nil {
		return nil, err
	}
	currentID, err := readS3PublishedSnapshotID(ctx, client, bucket)
	if err != nil {
		return nil, err
	}
	if currentID != published.SnapshotID {
		return nil, fmt.Errorf("S3 current snapshot %q differs from API snapshot %q", currentID, published.SnapshotID)
	}
	defer func() { _ = scaleDiagnosticsBackend(context.Background(), environment, 1) }()

	results := map[string]any{"publishedSnapshotID": published.SnapshotID}
	maintenanceBefore, err := readS3ScanCheckpoint(ctx, client, bucket, "maintenance-checkpoints/storage_inventory.json")
	if err != nil {
		return nil, fmt.Errorf("read storage inventory maintenance baseline: %w", err)
	}
	maintenanceScans := map[string]any{"baseline": maintenanceBefore}
	pendingID := "recovery-pending-" + environment.RunID
	if err := scaleDiagnosticsBackend(ctx, environment, 0); err != nil {
		return nil, err
	}
	if err := waitForStorageInventoryLeasesExpired(ctx, client, bucket); err != nil {
		return nil, err
	}
	pendingBatch, payload, err := seedResumablePendingBatch(ctx, client, bucket, pendingID, environment.RunID)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	if err := scaleDiagnosticsBackend(ctx, environment, 1); err != nil {
		return nil, err
	}
	resumed, err := waitForSeededPendingBatch(ctx, client, bucket, pendingID)
	if err != nil {
		return nil, err
	}
	if resumed.Snapshot.SnapshotID != pendingID || resumed.UsageRuns < 1 || resumed.UsagePending != nil || resumed.AttributionPhase != 3 {
		return nil, fmt.Errorf("seeded pending batch did not resume in place: %+v", resumed)
	}
	results["pendingRecoveryUsageRuns"] = resumed.UsageRuns
	pageKey, err := diagnosticsS3ObjectKey(pendingBatch.PageKey)
	if err != nil {
		return nil, err
	}
	page, err := readS3Object(ctx, client, bucket, pageKey)
	if err != nil || !reflect.DeepEqual(page, payload) {
		return nil, fmt.Errorf("resumed usage page differs from its staged payload: %v", err)
	}
	manifestKey, err := diagnosticsS3ObjectKey(pendingBatch.ManifestKey)
	if err != nil {
		return nil, err
	}
	manifest, err := readS3Object(ctx, client, bucket, manifestKey)
	if err != nil || string(manifest) != `{"pages":1,"rows":1}` {
		return nil, fmt.Errorf("resumed usage manifest is invalid: %v", err)
	}
	results["pendingBuildID"] = resumed.Snapshot.SnapshotID
	results["pendingRecoverySeconds"] = time.Since(started).Seconds()
	currentID, err = readS3PublishedSnapshotID(ctx, client, bucket)
	if err != nil || currentID != published.SnapshotID {
		return nil, fmt.Errorf("pending batch recovery changed published pointer from %q to %q: %v", published.SnapshotID, currentID, err)
	}
	if err := verifyRetainedStorageSummary(ctx, environment, published); err != nil {
		return nil, err
	}
	if err := scaleDiagnosticsBackend(ctx, environment, 0); err != nil {
		return nil, err
	}

	retainedReports := []report{published}
	for _, seed := range []struct {
		name         string
		withManifest bool
	}{
		{name: "legacy-page-only", withManifest: false},
		{name: "legacy-page-and-manifest", withManifest: true},
	} {
		buildID := seed.name + "-" + environment.RunID
		if err := waitForStorageInventoryLeasesExpired(ctx, client, bucket); err != nil {
			return nil, err
		}
		if err := seedLegacyStorageOutput(ctx, client, bucket, buildID, seed.withManifest); err != nil {
			return nil, err
		}
		started = time.Now()
		if err := scaleDiagnosticsBackend(ctx, environment, 1); err != nil {
			return nil, err
		}
		replacement, err := waitForLegacyStorageReplacement(ctx, client, bucket, buildID)
		if err != nil {
			return nil, err
		}
		if replacement.Snapshot.SnapshotID == buildID || replacement.AutomaticRestarts != 1 || replacement.RecoveredFromBuildID != buildID {
			return nil, fmt.Errorf("legacy build %s was not replaced exactly once: %+v", buildID, replacement)
		}
		results[seed.name+"ReplacementBuildID"] = replacement.Snapshot.SnapshotID
		currentID, err = readS3PublishedSnapshotID(ctx, client, bucket)
		previous := retainedReports[len(retainedReports)-1]
		if err != nil || currentID != previous.SnapshotID {
			return nil, fmt.Errorf("legacy recovery changed published pointer before replacement report completed: %q: %v", currentID, err)
		}
		if err := verifyRetainedStorageSummary(ctx, environment, previous); err != nil {
			return nil, err
		}
		fresh, err := awaitNewStorageReport(ctx, environment, replacement.Snapshot.SnapshotID, started)
		if err != nil {
			return nil, fmt.Errorf("legacy build %s replacement report: %w", buildID, err)
		}
		maintenanceStatus, err := waitForStorageInventoryScan(ctx, client, bucket, started)
		if err != nil {
			return nil, fmt.Errorf("legacy build %s maintenance scan: %w", buildID, err)
		}
		if fresh.SnapshotID != replacement.Snapshot.SnapshotID {
			return nil, fmt.Errorf("published replacement snapshot %q differs from checkpoint build %q", fresh.SnapshotID, replacement.Snapshot.SnapshotID)
		}
		objects, err := verifyObjects(ctx, environment, fresh)
		if err != nil {
			return nil, err
		}
		groups, err := verifyUsage(ctx, environment, fresh)
		if err != nil {
			return nil, err
		}
		logicalBytes, err := verifyFixtureLogicalBytes(ctx, environment, fresh)
		if err != nil {
			return nil, err
		}
		if err := verifyRegistryFiles(ctx, environment, fresh); err != nil {
			return nil, err
		}
		results[seed.name+"RecoverySeconds"] = time.Since(started).Seconds()
		results[seed.name+"PublishedSnapshotID"] = fresh.SnapshotID
		results[seed.name+"PhysicalObjects"] = objects
		results[seed.name+"UsageGroups"] = groups
		results[seed.name+"FixtureLogicalBytes"] = logicalBytes
		maintenanceScans[seed.name] = map[string]any{
			"recoveryStartedAt": started.UTC(), "reportCompletedAt": fresh.CompletedAt,
			"recoveryElapsedSeconds": time.Since(started).Seconds(), "scan": maintenanceStatus,
		}
		retainedReports = append(retainedReports, fresh)
		if err := scaleDiagnosticsBackend(ctx, environment, 0); err != nil {
			return nil, err
		}
	}
	if err := scaleDiagnosticsBackend(ctx, environment, 1); err != nil {
		return nil, err
	}
	currentID, err = readS3PublishedSnapshotID(ctx, client, bucket)
	latest := retainedReports[len(retainedReports)-1]
	if err != nil || currentID != latest.SnapshotID {
		return nil, fmt.Errorf("seed recovery published pointer %q differs from latest replacement %q: %v", currentID, latest.SnapshotID, err)
	}
	for _, retained := range retainedReports[:len(retainedReports)-1] {
		if err := verifyRetainedStorageSummary(ctx, environment, retained); err != nil {
			return nil, err
		}
	}
	dueRetryMeasurement, err := measureReceiptCleanupDueRetry(ctx, client, bucket, environment.RunID)
	if err != nil {
		return nil, err
	}
	retainedIDs := make([]string, 0, len(retainedReports)-1)
	for _, retained := range retainedReports[:len(retainedReports)-1] {
		retainedIDs = append(retainedIDs, retained.SnapshotID)
	}
	results["retainedPublishedSnapshots"] = retainedIDs
	results["latestPublishedSnapshotID"] = currentID
	results["storageInventoryMaintenanceScans"] = maintenanceScans
	results["receiptCleanupDueRetry"] = dueRetryMeasurement
	return results, nil
}

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

func diagnosticsS3Client(ctx context.Context, environment testkit.CaseContext) (*s3.Client, string, error) {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+diagnosticsS3SecretSuffix)
	if err != nil {
		return nil, "", err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" {
		return nil, "", fmt.Errorf("diagnostics S3 credential Secret ownership is invalid")
	}
	accessKey, secretKey := string(data["AWS_ACCESS_KEY_ID"]), string(data["AWS_SECRET_ACCESS_KEY"])
	if accessKey == "" || secretKey == "" {
		return nil, "", fmt.Errorf("diagnostics S3 credential Secret is incomplete")
	}
	serviceName := environment.Release + diagnosticsS3SecretSuffix
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return nil, "", err
	}
	clusterIP := testkit.String(testkit.Map(service["spec"])["clusterIP"])
	if clusterIP == "" || clusterIP == "None" {
		return nil, "", fmt.Errorf("diagnostics S3 fixture has no ClusterIP")
	}
	config, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithHTTPClient(&http.Client{Timeout: 20 * time.Second}),
	)
	if err != nil {
		return nil, "", err
	}
	client := s3.NewFromConfig(config, func(options *s3.Options) {
		options.BaseEndpoint = aws.String("http://" + clusterIP + ":8080")
		options.UsePathStyle = true
	})
	return client, "rh-" + environment.RunID, nil
}

func seedResumablePendingBatch(ctx context.Context, client *s3.Client, bucket, snapshotID, runID string) (seededPendingBatch, []byte, error) {
	rows := []seededUsageRow{{Domain: "state", Dimension: "category", Name: "seeded", ObjectBytes: 13, Objects: 1, LogicalUnknown: true, Attribution: "seeded pending batch"}}
	payload, err := json.Marshal(rows)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	manifest := seededUsageManifest{Pages: 1, Rows: len(rows)}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	sum := sha256.Sum256(payload)
	payloadDigest := hex.EncodeToString(sum[:])
	manifestSum := sha256.Sum256(manifestData)
	manifestDigest := hex.EncodeToString(manifestSum[:])
	run := seededUsageRun{Stage: "reduce"}
	batchID := storageSeedID("batch", runID)
	storageBase := diagnosticsStorageKeyPrefix + "snapshots/" + snapshotID + "/usage/"
	objectBase := diagnosticsS3ObjectPrefix + "snapshots/" + snapshotID + "/usage/"
	runPrefix := seededUsageRunPrefix(diagnosticsStorageKeyPrefix, snapshotID, run)
	pending := seededPendingBatch{
		Version: 1, BatchID: batchID, SnapshotID: snapshotID, Run: run,
		NextCursor: seededCursor{Done: true}, NextPhase: 3,
		PayloadKey: storageBase + "pending/" + batchID + ".json", PayloadSHA256: payloadDigest,
		PageKey: runPrefix + "00000000.json", PageSHA256: payloadDigest,
		ManifestKey: runPrefix + "manifest.json", Manifest: manifest, ManifestSHA256: manifestDigest,
		PreparedAt: time.Now().UTC(),
	}
	state := newSeededStorageCollection(snapshotID)
	state.AttributionPhase = 2
	state.UsagePending = &pending
	if err := putS3Object(ctx, client, bucket, objectBase+"pending/"+batchID+".json", payload); err != nil {
		return seededPendingBatch{}, nil, err
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return seededPendingBatch{}, nil, err
	}
	if err := putS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"checkpoint.json", encoded); err != nil {
		return seededPendingBatch{}, nil, err
	}
	if err := seedStorageInventoryDue(ctx, client, bucket); err != nil {
		return seededPendingBatch{}, nil, err
	}
	return pending, payload, nil
}

func seedLegacyStorageOutput(ctx context.Context, client *s3.Client, bucket, snapshotID string, withManifest bool) error {
	state := newSeededStorageCollection(snapshotID)
	state.AttributionPhase = 2
	checkpoint, err := json.Marshal(state)
	if err != nil {
		return err
	}
	runPrefix := seededUsageRunPrefix(diagnosticsS3ObjectPrefix, snapshotID, seededUsageRun{Stage: "reduce"})
	if err := putS3Object(ctx, client, bucket, runPrefix+"00000000.json", []byte("[]")); err != nil {
		return err
	}
	if withManifest {
		if err := putS3Object(ctx, client, bucket, runPrefix+"manifest.json", []byte(`{"pages":1,"rows":0}`)); err != nil {
			return err
		}
	}
	if err := putS3Object(ctx, client, bucket, diagnosticsS3ObjectPrefix+"checkpoint.json", checkpoint); err != nil {
		return err
	}
	return seedStorageInventoryDue(ctx, client, bucket)
}

func seedStorageInventoryDue(ctx context.Context, client *s3.Client, bucket string) error {
	checkpoint, err := json.Marshal(seededMaintenanceCheckpoint{})
	if err != nil {
		return err
	}
	return putS3Object(ctx, client, bucket, diagnosticsS3Maintenance, checkpoint)
}

func seededUsageRunPrefix(prefix, snapshotID string, run seededUsageRun) string {
	return fmt.Sprintf("%ssnapshots/%s/usage/%s/%04d/%08d/", prefix, snapshotID, run.Stage, run.Level, run.Index)
}

func diagnosticsS3ObjectKey(storageKey string) (string, error) {
	relative, found := strings.CutPrefix(storageKey, diagnosticsStorageKeyPrefix)
	if !found {
		return "", fmt.Errorf("storage object key %q does not use prefix %q", storageKey, diagnosticsStorageKeyPrefix)
	}
	return diagnosticsS3ObjectPrefix + relative, nil
}

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

func storageSeedID(kind, value string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + value))
	return hex.EncodeToString(sum[:16])
}
