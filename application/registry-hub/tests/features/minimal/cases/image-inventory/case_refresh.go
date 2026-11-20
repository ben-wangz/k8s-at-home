package imageinventory

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func awaitUnchangedRefresh(ctx context.Context, environment testkit.CaseContext, query url.Values, snapshot string, beforeStatus inventoryStatus, beforeMetrics inventoryMetrics) (map[string]any, error) {
	deadline := time.Now().Add(12 * time.Minute)
	baselineStatus, baselineMetrics, baselineSnapshot := beforeStatus, beforeMetrics, snapshot
	lastObservedStatus := beforeStatus
	replacementPublications := 0
	for time.Now().Before(deadline) {
		status, err := readInventoryStatus(ctx, environment)
		if err != nil {
			return nil, err
		}
		lastObservedStatus = status
		if status.LastSourceCheck.After(baselineStatus.LastSourceCheck) && !status.Building {
			page, err := read(ctx, environment, "/api/v1/images?"+query.Encode())
			if err != nil {
				return nil, err
			}
			afterMetrics, err := readInventoryMetrics(ctx, environment)
			if err != nil {
				return nil, err
			}
			currentSnapshot := testkit.String(page["snapshotID"])
			if currentSnapshot != baselineSnapshot || !status.LastPublishedAt.Equal(baselineStatus.LastPublishedAt) {
				baselineStatus, baselineMetrics, baselineSnapshot = status, afterMetrics, currentSnapshot
				replacementPublications++
				continue
			}
			if afterMetrics.registryAttempts != baselineMetrics.registryAttempts || afterMetrics.scanRounds != baselineMetrics.scanRounds || afterMetrics.snapshotWrites != baselineMetrics.snapshotWrites || afterMetrics.snapshotWriteBytes != baselineMetrics.snapshotWriteBytes {
				return nil, fmt.Errorf("unchanged source refresh performed registry work or immutable snapshot writes: before=%+v after=%+v", baselineMetrics, afterMetrics)
			}
			if afterMetrics.s3AttemptsObserved != baselineMetrics.s3AttemptsObserved {
				return nil, fmt.Errorf("S3 attempt metric sample state changed across the source refresh")
			}
			if !afterMetrics.refreshStorageOperationsObserved {
				return nil, fmt.Errorf("inventory refresh storage operation metrics were not observed")
			}
			storageDelta := afterMetrics.refreshStorageOperations - baselineMetrics.refreshStorageOperations
			allStorageDelta := afterMetrics.storageOperations - baselineMetrics.storageOperations
			s3Delta := afterMetrics.refreshS3Attempts - baselineMetrics.refreshS3Attempts
			allS3Delta := afterMetrics.s3Attempts - baselineMetrics.s3Attempts
			if storageDelta < 0 || storageDelta > 256 {
				return nil, fmt.Errorf("unchanged source refresh used %.0f refresh storage operations (budget 0..256) and %.0f refresh S3 HTTP attempts; all inventory-builder deltas: storage=%.0f S3=%.0f; source check %s after baseline %s with %d replacement publications; refresh operation deltas: %s; all operation deltas: %s", storageDelta, s3Delta, allStorageDelta, allS3Delta, status.LastSourceCheck.Format(time.RFC3339Nano), baselineStatus.LastSourceCheck.Format(time.RFC3339Nano), replacementPublications, strings.Join(storageWorkDelta(baselineMetrics.refreshStorageWorkByLabel, afterMetrics.refreshStorageWorkByLabel), "; "), strings.Join(storageWorkDelta(baselineMetrics.storageWorkByLabel, afterMetrics.storageWorkByLabel), "; "))
			}
			if s3Delta < 0 || s3Delta > 512 {
				return nil, fmt.Errorf("unchanged source refresh used %.0f refresh S3 HTTP attempts (budget 0..512); all inventory-builder S3 delta was %.0f", s3Delta, allS3Delta)
			}
			freshness, err := verifyFreshnessAgreement(ctx, environment, query)
			if err != nil {
				return nil, err
			}
			return map[string]any{
				"freshness":                              freshness,
				"sourceCheckAt":                          status.LastSourceCheck,
				"inventoryRefreshStorageOperations":      storageDelta,
				"inventoryRefreshStorageOperationDeltas": storageWorkDelta(baselineMetrics.refreshStorageWorkByLabel, afterMetrics.refreshStorageWorkByLabel),
				"inventoryBuilderStorageOperations":      allStorageDelta,
				"inventoryBuilderStorageOperationDeltas": storageWorkDelta(baselineMetrics.storageWorkByLabel, afterMetrics.storageWorkByLabel),
				"inventoryRefreshS3HTTPAttempts":         s3Delta,
				"inventoryBuilderS3HTTPAttempts":         allS3Delta,
				"s3HTTPAttemptsObserved":                 afterMetrics.s3AttemptsObserved,
				"snapshotIDPreserved":                    baselineSnapshot,
				"replacementPublicationsBefore":          replacementPublications,
				"immutableSnapshotWrites":                afterMetrics.snapshotWrites - baselineMetrics.snapshotWrites,
				"registryHTTPAttemptsAdded":              afterMetrics.registryAttempts - baselineMetrics.registryAttempts,
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("unchanged inventory refresh did not stabilize within twelve minutes (baseline=%s latest=%s building=%t replacementPublications=%d)",
		beforeStatus.LastSourceCheck.Format(time.RFC3339Nano), lastObservedStatus.LastSourceCheck.Format(time.RFC3339Nano), lastObservedStatus.Building, replacementPublications)
}
