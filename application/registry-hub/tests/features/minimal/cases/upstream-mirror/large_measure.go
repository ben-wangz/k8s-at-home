package upstreammirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func measureLargeLayer(run *mirrorRun, mib int) (result map[string]any, runErr error) {
	ctx, cancel := context.WithTimeout(run.context, 30*time.Minute)
	defer cancel()
	environment := run.environment
	prefix := fmt.Sprintf("%d-mib", mib)
	defer func() {
		evidence, done := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer done()
		runErr = errors.Join(runErr, largeEvidence(evidence, environment, prefix+"-after"))
	}()
	blocker, err := prepareLargeBlocker(ctx, run, mib)
	if err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("large-layer-%d", mib)
	manifestStart := time.Now()
	body, _, err := support.ReadManifest(ctx, run.registry, run.remoteRepository, ref, support.OCIManifestMediaType)
	manifestSeconds := time.Since(manifestStart).Seconds()
	if err != nil {
		return nil, err
	}
	var manifest support.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("decode large-layer manifest: %w", err)
	}
	if len(manifest.Layers) != 1 || manifest.Layers[0].Size != mib<<20 {
		return nil, fmt.Errorf("unexpected layer count or size for %d MiB manifest", mib)
	}
	if _, err := WaitForTaskStatus(ctx, environment, ref, "waiting_capacity", 3*time.Minute); err != nil {
		return nil, err
	}
	if err := setBackendEnvironment(ctx, environment, "REGISTRY_HUB_WORKERS=4"); err != nil {
		return nil, err
	}
	if err := largeEvidence(ctx, environment, prefix+"-before-cold"); err != nil {
		return nil, err
	}
	layer := manifest.Layers[0]
	read := func(phase string) (float64, error) {
		var duration float64
		err := testkit.RunPhase(environment, prefix+"-"+phase, func() error {
			var readErr error
			duration, readErr = readLargeLayer(ctx, run.registry, run.remoteRepository, layer)
			return readErr
		})
		return duration, err
	}
	coldSeconds, err := read("cold-client")
	if err != nil {
		return nil, fmt.Errorf("cold read: %w", err)
	}
	waiting, err := WaitForTaskStatus(ctx, environment, layer.Digest, "waiting_capacity", 3*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("wait for cache-ready layer: %w", err)
	}
	if testkit.String(waiting["waitReason"]) != "distribution_quota" {
		return nil, fmt.Errorf("large layer not waiting for Distribution capacity: %v", waiting)
	}
	cacheReady := time.Now().UTC()
	if err := largeEvidence(ctx, environment, prefix+"-before-cache"); err != nil {
		return nil, err
	}
	if err := requireLargeReadSource(environment, prefix+"-before-cold", prefix+"-before-cache", "upstream"); err != nil {
		return nil, err
	}
	cachedSeconds, err := read("cached-client")
	if err != nil {
		return nil, fmt.Errorf("cached read: %w", err)
	}
	if err := requireLargeUpstreamGETs(ctx, run, layer.Digest, 1); err != nil {
		return nil, err
	}
	if err := largeEvidence(ctx, environment, prefix+"-before-release"); err != nil {
		return nil, err
	}
	if err := requireLargeReadSource(environment, prefix+"-before-cache", prefix+"-before-release", "cache"); err != nil {
		return nil, err
	}
	released := time.Now().UTC()
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/tasks/"+testkit.String(blocker["id"])+"/cancel", nil, nil)
	if err != nil {
		return nil, err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cancel capacity blocker returned %s", response.Status)
	}
	var root map[string]any
	err = testkit.RunPhase(environment, prefix+"-anchor-wait", func() error {
		var waitErr error
		root, waitErr = WaitForTaskStatus(ctx, environment, ref, "solidified", 20*time.Minute)
		return waitErr
	})
	if err != nil {
		return nil, fmt.Errorf("wait for anchored platform: %w", err)
	}
	if root["rootPublished"] != true {
		return nil, fmt.Errorf("completed platform was not published: %v", root)
	}
	observed := time.Now().UTC()
	finished, err := WaitForTaskStatus(ctx, environment, layer.Digest, "solidified", time.Minute)
	if err != nil {
		return nil, err
	}
	if err := largeEvidence(ctx, environment, prefix+"-anchored"); err != nil {
		return nil, err
	}
	distributionSeconds, err := read("distribution-client")
	if err != nil {
		return nil, fmt.Errorf("Distribution read: %w", err)
	}
	if err := requireLargeUpstreamGETs(ctx, run, layer.Digest, 1); err != nil {
		return nil, err
	}
	if err := largeEvidence(ctx, environment, prefix+"-after-distribution"); err != nil {
		return nil, err
	}
	if err := requireLargeReadSource(environment, prefix+"-anchored", prefix+"-after-distribution", "local"); err != nil {
		return nil, err
	}
	return map[string]any{
		"layerMiB": mib, "bytes": layer.Size, "digest": layer.Digest,
		"firstManifestSeconds": manifestSeconds, "coldLayerSeconds": coldSeconds,
		"cachedLayerSeconds": cachedSeconds, "distributionLayerSeconds": distributionSeconds,
		"cacheReadyObservedAt": cacheReady, "capacityReleaseRequestedAt": released, "anchorObservedAt": observed,
		"cacheReadyToAnchorSeconds": observed.Sub(cacheReady).Seconds(), "intentionalHoldSeconds": released.Sub(cacheReady).Seconds(), "releaseToAnchorSeconds": observed.Sub(released).Seconds(),
		"rootTask": root, "layerTask": finished, "upstreamLayerGETs": 1, "requestSourcesVerified": []string{"upstream", "cache", "local"},
	}, nil
}
