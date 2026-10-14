package upstreammirror

import (
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifySupplementalCases(run *mirrorRun) (map[string]any, error) {
	checks := []func() (map[string]any, error){
		func() (map[string]any, error) {
			return verifyInlineAndURLDescriptors(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyInvalidInlineData(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyUpstreamStatusMatrix(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyUpstreamRetryAfter(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyBackgroundRetryRecovery(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyNestedPlatformClosure(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyClosureDepthLimit(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifySuccessfulHead(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifySharedDigestConflicts(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyPartialUpstreamBlob(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
	}
	result := make(map[string]any)
	for _, check := range checks {
		values, err := check()
		if err != nil {
			return nil, err
		}
		mergeMirrorResults(result, values)
	}
	return result, nil
}

func verifyMultipartProbe(run *mirrorRun) (map[string]any, error) {
	manifestBytes, _, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, "minimal-multipart", support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("multipart probe manifest read failed: %w", err)
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || len(manifest.Layers) != 1 {
		return nil, fmt.Errorf("multipart probe manifest is invalid")
	}
	task, err := waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-multipart")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["rootDigest"]) != support.Digest(manifestBytes) || task["rootPublished"] != true || testkit.String(task["status"]) != "solidified" {
		return nil, fmt.Errorf("multipart probe manifest did not solidify")
	}
	blob, err := support.ReadBlob(run.context, run.registry, run.remoteRepository, manifest.Layers[0].Digest)
	if err != nil {
		return nil, fmt.Errorf("multipart probe blob is not locally readable: %w", err)
	}
	if len(blob) != manifest.Layers[0].Size || support.Digest(blob) != manifest.Layers[0].Digest {
		return nil, fmt.Errorf("multipart probe blob failed size or digest validation")
	}
	return map[string]any{
		"multipartProbeTaskID":         testkit.String(task["id"]),
		"multipartProbeManifestDigest": support.Digest(manifestBytes),
		"multipartProbeBlobDigest":     manifest.Layers[0].Digest,
		"multipartProbeBlobSize":       len(blob),
	}, nil
}

func verifyLimitCases(run *mirrorRun) (map[string]any, error) {
	checks := []func() (map[string]any, error){
		func() (map[string]any, error) {
			return verifyClosureLimit(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository, "minimal-object-limit", "closureObjectLimit", 3, "REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS=3")
		},
		func() (map[string]any, error) {
			return verifyClosureLimit(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository, "minimal-byte-limit", "closureByteLimit", 1, "REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS=10000", "REGISTRY_HUB_LIMIT_CLOSURE_BYTES=1")
		},
		func() (map[string]any, error) {
			return verifyObjectSizeLimit(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyRequestDeadline(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
		func() (map[string]any, error) {
			return verifyHeadRequestDeadline(run.context, run.environment, run.registry, run.remoteRepository, run.upstreamRepository)
		},
	}
	result := make(map[string]any)
	for _, check := range checks {
		values, err := check()
		if err != nil {
			return nil, err
		}
		mergeMirrorResults(result, values)
	}
	return result, nil
}

func mergeMirrorResults(target map[string]any, sources ...map[string]any) {
	for _, source := range sources {
		for key, value := range source {
			target[key] = value
		}
	}
}
