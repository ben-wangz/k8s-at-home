package gc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
	"time"
)

func verifySolidificationCancellationGC(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string) (map[string]any, error) {
	referenceHash := strings.TrimPrefix(support.Digest([]byte(environment.RunID)), "sha256:")
	reference := "gc-cancel-" + referenceHash[:16]
	startedAt := time.Now().UTC()
	manifestBytes, _, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("read cancellation fixture manifest: %w", err)
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Config == nil || len(manifest.Layers) != 1 || manifest.Config.Data == "" {
		return nil, fmt.Errorf("cancellation fixture manifest is incomplete")
	}
	configBytes, err := base64.StdEncoding.DecodeString(manifest.Config.Data)
	if err != nil || len(configBytes) != manifest.Config.Size || support.Digest(configBytes) != manifest.Config.Digest {
		return nil, fmt.Errorf("cancellation fixture inline config is invalid")
	}
	clientBytes, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("inline config read failed: %w", err)
	}
	if !bytes.Equal(clientBytes, configBytes) {
		return nil, fmt.Errorf("inline config read returned unexpected bytes")
	}
	tasks, err := waitForProtectedSolidification(ctx, environment, repository, reference, manifest.Config.Digest, startedAt)
	if err != nil {
		return nil, err
	}
	cacheCleanup, err := cleanupSolidificationCache(ctx, environment)
	if err != nil {
		return nil, err
	}
	blocked, err := rejectGCWhileSolidificationProtected(ctx, environment)
	if err != nil {
		return nil, err
	}
	cancelled, err := cancelSolidificationTasks(ctx, environment, repository, reference, manifest.Config.Digest, startedAt)
	if err != nil {
		return nil, err
	}
	gate, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(gate); err != nil {
		return nil, err
	}
	gcStarted, err := startGC(ctx, environment)
	if err != nil {
		return nil, err
	}
	gcFinished, err := waitForGC(ctx, environment)
	if err != nil {
		return nil, err
	}
	if testkit.String(gcFinished["id"]) != testkit.String(gcStarted["id"]) || testkit.String(gcFinished["state"]) != "succeeded" {
		return nil, fmt.Errorf("GC after cancellation did not complete successfully")
	}
	gcJob, err := verifyGCJob(ctx, environment, gcStarted, gcFinished)
	if err != nil {
		return nil, err
	}
	if err := cleanupGCJob(ctx, environment, testkit.String(gcJob["name"]), testkit.String(gcJob["uid"])); err != nil {
		return nil, err
	}
	readSince := time.Now().UTC()
	upstreamBefore, err := fixtureBlobRequestCount(ctx, environment, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	refetched, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("post-GC client blob read failed: %w", err)
	}
	if !bytes.Equal(refetched, configBytes) {
		return nil, fmt.Errorf("post-GC client blob read returned unexpected bytes")
	}
	upstreamAfter, err := fixtureBlobRequestCount(ctx, environment, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	distributionMisses, err := distributionBlobMissCount(ctx, environment, manifest.Config.Digest, readSince)
	if err != nil {
		return nil, err
	}
	if upstreamAfter != upstreamBefore+1 || distributionMisses == 0 {
		return nil, fmt.Errorf("post-GC read did not prove Distribution and cache misses: upstream requests %d -> %d, Distribution misses %d", upstreamBefore, upstreamAfter, distributionMisses)
	}
	newBlobTask, err := waitForNewBlobTask(ctx, environment, repository, manifest.Config.Digest, readSince, testkit.String(testkit.Map(tasks["blobTask"])["roundID"]))
	if err != nil {
		return nil, err
	}
	newBlobTaskID := testkit.String(newBlobTask["id"])
	newBlobTask, err = cancelAndWaitForTask(ctx, environment, newBlobTaskID)
	if err != nil {
		return nil, err
	}
	returnStatus, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(returnStatus); err != nil {
		return nil, err
	}
	return map[string]any{
		"reference": reference, "taskID": testkit.String(testkit.Map(tasks["rootTask"])["id"]), "taskStatusAfterCancel": "cancelled",
		"cancelResponseStatus": testkit.Int(cancelled["cancelResponseStatus"]), "cancelledTasks": cancelled["taskIDs"],
		"cancelResponses": cancelled["responses"],
		"duplicateCacheEntriesRemovedWhileProtected": testkit.Int(cacheCleanup["cacheRemoved"]),
		"protectionSessionBlockedGC":                 blocked, "cancellationQuiesced": cancelled["quiesced"] == true,
		"gcGateOpenAfterCancellation": true,
		"gcAfterCancelID":             testkit.String(gcFinished["id"]), "unanchoredBlobDigest": manifest.Config.Digest,
		"distributionBlobMissesAfterGC": distributionMisses, "upstreamBlobRequestsBefore": upstreamBefore,
		"upstreamBlobRequestsAfter": upstreamAfter, "cacheAndDistributionMissRecoveredFromUpstream": true,
		"newSolidificationRoundAfterRefetch": true, "newBlobTaskID": newBlobTaskID,
		"newBlobTaskStatusAfterCleanup": testkit.String(newBlobTask["status"]),
	}, nil
}

func waitForProtectedSolidification(ctx context.Context, environment testkit.CaseContext, repository, reference, digest string, since time.Time) (map[string]any, error) {
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		rootTask, err := taskDetailForReference(ctx, environment, repository, reference)
		if err == nil {
			status := testkit.String(rootTask["status"])
			if status == "solidified" || status == "failed" || status == "cancelled" {
				return nil, fmt.Errorf("cancellation fixture task ended in %s before the protected state", status)
			}
			if rootTask["rootPublished"] == true || testkit.String(rootTask["rootDigest"]) == "" {
				return nil, fmt.Errorf("cancellation fixture root was published or not discovered")
			}
			blobTask, blobErr := taskDetailForBlobDigest(ctx, environment, repository, digest, since, "")
			if blobErr != nil && !strings.Contains(blobErr.Error(), "no matching blob task") {
				return nil, blobErr
			}
			unanchored, err := hasQuiescentUnanchoredSession(rootTask)
			if err != nil {
				return nil, err
			}
			if !unanchored && blobTask != nil {
				unanchored, err = hasQuiescentUnanchoredSession(blobTask)
				if err != nil {
					return nil, err
				}
			}
			localReads, err := distributionBlobReadCount(ctx, environment, digest, since)
			if err != nil {
				return nil, err
			}
			if unanchored && localReads > 0 && blobTask != nil && testkit.String(blobTask["status"]) == "verified_local" {
				return map[string]any{"rootTask": rootTask, "blobTask": blobTask}, nil
			}
		} else if !strings.Contains(err.Error(), "no matching task") {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("cancellation fixture did not reach a quiescent unanchored state")
		case <-time.After(500 * time.Millisecond):
		}
	}
}
