package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
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
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Config == nil || len(manifest.Layers) != 1 || manifest.Config.Data != "" {
		return nil, fmt.Errorf("cancellation fixture manifest is incomplete")
	}
	clientBytes, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("config blob read failed: %w", err)
	}
	if len(clientBytes) != manifest.Config.Size || support.Digest(clientBytes) != manifest.Config.Digest {
		return nil, fmt.Errorf("config blob read returned unexpected bytes")
	}
	tasks, err := waitForProtectedSolidification(ctx, environment, repository, reference, manifest.Config.Digest, startedAt)
	if err != nil {
		return nil, err
	}
	cacheCleanup, err := cleanupSolidificationCache(ctx, environment, testkit.String(testkit.Map(tasks["rootTask"])["id"]), manifest.Config.Digest)
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
	if err := verifyCleanupSummariesAfterRestart(ctx, environment, cancelled); err != nil {
		return nil, err
	}
	gate, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(gate); err != nil {
		return nil, err
	}
	if err := waitForRegistryService(ctx, registry); err != nil {
		return nil, fmt.Errorf("Registry Service did not recover after backend restart: %w", err)
	}
	oldRootTask := testkit.Map(tasks["rootTask"])
	oldBlobTask := testkit.Map(tasks["blobTask"])
	deleteResponse, err := registry.Delete(ctx, repository, "/blobs/"+manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	deleteStatus := deleteResponse.StatusCode
	deleteResponse.Body.Close()
	if deleteStatus != http.StatusAccepted {
		return nil, fmt.Errorf("authorized Hub blob DELETE returned %d, want 202", deleteStatus)
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
	if err := waitForRegistryService(ctx, registry); err != nil {
		return nil, fmt.Errorf("Registry Service did not recover after post-cancellation GC: %w", err)
	}
	readSince := time.Now().UTC()
	upstreamBefore, err := fixtureBlobRequestCount(ctx, environment, manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	podmanPull, err := podmanPullPinnedImage(ctx, environment, registry.Basic, repository, testkit.String(oldRootTask["rootDigest"]))
	if err != nil {
		evidence := podmanPullFailureDiagnostics(ctx, environment, registry, repository, manifest, readSince, err)
		encoded, _ := json.Marshal(evidence)
		return nil, fmt.Errorf("%w; failure evidence: %s", err, encoded)
	}
	repulledManifest, manifestHeaders, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil || !bytes.Equal(repulledManifest, manifestBytes) {
		return nil, fmt.Errorf("post-GC root manifest read did not preserve the pinned fixture: %v", err)
	}
	rootDigest := support.Digest(repulledManifest)
	if manifestHeaders.Get("Docker-Content-Digest") != rootDigest {
		return nil, fmt.Errorf("post-GC manifest digest header %q does not match %q", manifestHeaders.Get("Docker-Content-Digest"), rootDigest)
	}
	refetched, err := support.ReadBlob(ctx, registry, repository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("post-GC client blob read failed: %w", err)
	}
	if !bytes.Equal(refetched, clientBytes) {
		return nil, fmt.Errorf("post-GC client blob read returned unexpected bytes")
	}
	for _, layer := range manifest.Layers {
		layerBytes, err := support.ReadBlob(ctx, registry, repository, layer.Digest)
		if err != nil || len(layerBytes) != layer.Size || support.Digest(layerBytes) != layer.Digest {
			return nil, fmt.Errorf("post-GC layer %s did not pass client digest verification: %v", layer.Digest, err)
		}
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
	oldBlobGeneration, newBlobGeneration := testkit.Int(oldBlobTask["generation"]), testkit.Int(newBlobTask["generation"])
	if newBlobGeneration <= oldBlobGeneration || testkit.String(newBlobTask["roundID"]) == testkit.String(oldBlobTask["roundID"]) {
		return nil, fmt.Errorf("authorized DELETE did not create a new blob generation and round: %d -> %d", oldBlobGeneration, newBlobGeneration)
	}
	newRootTask, err := waitForNewRootCompletion(ctx, environment, repository, reference, readSince,
		testkit.String(oldRootTask["id"]), testkit.String(oldRootTask["roundID"]))
	if err != nil {
		diagnosticCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		evidence := rootCompletionFailureDiagnostics(diagnosticCtx, environment, repository, reference, readSince,
			testkit.String(oldRootTask["id"]), testkit.String(oldRootTask["roundID"]), manifest, podmanPull, newBlobTask, err)
		encoded, _ := json.Marshal(evidence)
		return nil, fmt.Errorf("%w; root completion failure evidence: %s", err, encoded)
	}
	returnStatus, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(returnStatus); err != nil {
		return nil, err
	}
	directDistributionRecovery, err := verifyDirectDistributionCleanupRecovery(ctx, environment, registry, repository, reference,
		manifestBytes, clientBytes, manifest, newRootTask, newBlobTask)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"reference": reference, "taskID": testkit.String(testkit.Map(tasks["rootTask"])["id"]), "taskStatusAfterCancel": "cancelled",
		"cancelResponseStatus": testkit.Int(cancelled["cancelResponseStatus"]), "cancelledTasks": cancelled["taskIDs"],
		"cancelResponses":                         cancelled["responses"],
		"cleanupSummaries":                        cancelled["cleanupSummaries"],
		"cleanupSummaryRestartVerified":           cancelled["cleanupSummaryRestartVerified"],
		"manualCacheEntriesRemovedWhileProtected": testkit.Int(cacheCleanup["cacheRemoved"]),
		"duplicateCacheMissingWhileProtected":     cacheCleanup["targetCacheMissing"] == true,
		"protectionSessionBlockedGC":              blocked, "cancellationQuiesced": cancelled["quiesced"] == true,
		"gcGateOpenAfterCancellation": true,
		"gcAfterCancelID":             testkit.String(gcFinished["id"]), "unanchoredBlobDigest": manifest.Config.Digest,
		"distributionBlobMissesAfterGC": distributionMisses, "upstreamBlobRequestsBefore": upstreamBefore,
		"upstreamBlobRequestsAfter": upstreamAfter, "cacheAndDistributionMissRecoveredFromUpstream": true,
		"hubAuthorizedDeleteStatus": deleteStatus, "deletePrincipal": "writer",
		"podmanDigestPull":  podmanPull,
		"oldBlobGeneration": oldBlobGeneration, "newBlobGeneration": newBlobGeneration,
		"newSolidificationRoundAfterRefetch": true, "newBlobTaskID": newBlobTaskID,
		"newBlobTaskStatusAfterRefetch": testkit.String(newBlobTask["status"]),
		"repulledRootTaskID":            testkit.String(newRootTask["id"]), "repulledRootRoundID": testkit.String(newRootTask["roundID"]),
		"repulledRootDigest": rootDigest, "repulledRootPublished": newRootTask["rootPublished"],
		"repulledRootTaskStatus":            testkit.String(newRootTask["status"]),
		"directDistributionCleanupRecovery": directDistributionRecovery,
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
			if unanchored && blobTask != nil && testkit.String(blobTask["status"]) == "verified_local" {
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
