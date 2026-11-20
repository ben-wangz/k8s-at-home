package gc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"time"
)

func verifyDirectDistributionCleanupRecovery(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository, reference string, manifestBytes, clientBytes []byte, manifest support.Manifest, priorRoot, priorBlob map[string]any) (map[string]any, error) {
	digest := manifest.Config.Digest
	cacheCleanup, err := cleanupSolidificationCache(ctx, environment, testkit.String(priorRoot["id"]), digest)
	if err != nil {
		return nil, fmt.Errorf("clear client cache before direct Distribution cleanup: %w", err)
	}
	upstreamBefore, err := fixtureBlobRequestCount(ctx, environment, digest)
	if err != nil {
		return nil, err
	}
	deletion, err := directDistributionDeleteBlob(ctx, environment, repository, digest)
	if err != nil {
		return nil, err
	}
	readSince := time.Now().UTC()
	podmanPull, err := podmanPullPinnedImage(ctx, environment, registry.Basic, repository, testkit.String(priorRoot["rootDigest"]))
	if err != nil {
		evidence := podmanPullFailureDiagnostics(ctx, environment, registry, repository, manifest, readSince, err)
		encoded, _ := json.Marshal(evidence)
		return nil, fmt.Errorf("direct Distribution cleanup repull failed: %w; failure evidence: %s", err, encoded)
	}
	refetchedManifest, headers, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil || !bytes.Equal(refetchedManifest, manifestBytes) {
		return nil, fmt.Errorf("direct Distribution cleanup changed the pinned root manifest: %v", err)
	}
	if headers.Get("Docker-Content-Digest") != support.Digest(manifestBytes) {
		return nil, fmt.Errorf("direct Distribution cleanup returned an unexpected manifest digest")
	}
	refetchedBlob, err := support.ReadBlob(ctx, registry, repository, digest)
	if err != nil || !bytes.Equal(refetchedBlob, clientBytes) {
		return nil, fmt.Errorf("direct Distribution cleanup did not restore the verified blob bytes: %v", err)
	}
	upstreamAfter, err := fixtureBlobRequestCount(ctx, environment, digest)
	if err != nil {
		return nil, err
	}
	distributionMisses, err := distributionBlobMissCount(ctx, environment, digest, readSince)
	if err != nil {
		return nil, err
	}
	if upstreamAfter != upstreamBefore+1 || distributionMisses == 0 {
		return nil, fmt.Errorf("direct Distribution cleanup did not trigger one upstream recovery: upstream %d -> %d, Distribution misses %d", upstreamBefore, upstreamAfter, distributionMisses)
	}
	newBlob, err := waitForNewBlobTask(ctx, environment, repository, digest, readSince, testkit.String(priorBlob["roundID"]))
	if err != nil {
		return nil, err
	}
	if testkit.String(newBlob["roundID"]) == testkit.String(priorBlob["roundID"]) {
		return nil, fmt.Errorf("direct Distribution cleanup reused the previous blob round")
	}
	newBlob, err = waitForBlobTaskSolidified(ctx, environment, newBlob)
	if err != nil {
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		evidence := directDistributionRecoveryFailureEvidence(diagnosticCtx, environment, repository, reference, readSince,
			priorRoot, manifest, podmanPull, newBlob, err)
		encoded, _ := json.Marshal(evidence)
		return nil, fmt.Errorf("direct Distribution cleanup blob recovery did not anchor: %w; failure evidence: %s", err, encoded)
	}
	gate, err := gcStatus(ctx, environment)
	if err != nil {
		return nil, err
	}
	if err := requireNoProtectionBlockers(gate); err != nil {
		diagnosticCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		evidence := directDistributionRecoveryFailureEvidence(diagnosticCtx, environment, repository, reference, readSince,
			priorRoot, manifest, podmanPull, newBlob, err)
		encoded, _ := json.Marshal(evidence)
		return nil, fmt.Errorf("protection remained after direct Distribution blob recovery: %w; failure evidence: %s", err, encoded)
	}
	return map[string]any{
		"clientCacheMissingBeforeDelete": cacheCleanup["targetCacheMissing"] == true,
		"distributionDelete":             deletion, "podmanDigestPull": podmanPull,
		"upstreamBlobRequestsBefore": upstreamBefore, "upstreamBlobRequestsAfter": upstreamAfter,
		"distributionMissesDuringRepull": distributionMisses,
		"previousBlobTaskID":             testkit.String(priorBlob["id"]), "newBlobTaskID": testkit.String(newBlob["id"]),
		"previousBlobRoundID": testkit.String(priorBlob["roundID"]), "newBlobRoundID": testkit.String(newBlob["roundID"]),
		"newBlobTaskStatus": testkit.String(newBlob["status"]), "verifiedBlobRestored": true,
		"rootManifestDigest": support.Digest(manifestBytes), "gcProtectionBlockers": 0,
	}, nil
}
