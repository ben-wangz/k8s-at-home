package diagnostics

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

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
