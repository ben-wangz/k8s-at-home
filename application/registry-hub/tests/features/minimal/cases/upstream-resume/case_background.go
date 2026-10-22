package upstreamresume

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyBackgroundRetry(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) (map[string]any, error) {
	started := time.Now()
	manifest, headers, err := support.ReadManifest(ctx, registry, "github.com/"+upstreamRepository, "resume-background", support.OCIManifestMediaType)
	if err != nil {
		return nil, err
	}
	manifestDigest := support.Digest(manifest)
	if headers.Get("Docker-Content-Digest") != manifestDigest {
		return nil, fmt.Errorf("background fixture manifest digest header is invalid")
	}
	var root support.Manifest
	if err := json.Unmarshal(manifest, &root); err != nil || root.Config == nil {
		return nil, fmt.Errorf("background fixture manifest has no valid config descriptor")
	}
	task, err := waitForBackgroundTask(ctx, environment, "waiting_dependencies")
	if err != nil {
		return nil, err
	}
	if task["rootPublished"] == true {
		return nil, fmt.Errorf("background fixture root was published before client blob demand")
	}
	lines, err := fixtureLinesIfAny(ctx, environment, "resume-background", started)
	if err != nil {
		return nil, err
	}
	if len(lines) != 0 {
		return nil, fmt.Errorf("manifest discovery fetched the background layer before client demand: %v", lines)
	}
	config, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, root.Config.Digest)
	if err != nil || support.Digest(config) != root.Config.Digest {
		return nil, fmt.Errorf("client-triggered background config read is invalid: err=%v size=%d", err, len(config))
	}
	expected, digest := buildResumeLayer("background")
	blob, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
	if err != nil || len(blob) != len(expected) || support.Digest(blob) != digest {
		return nil, fmt.Errorf("client-triggered background layer is invalid: err=%v size=%d", err, len(blob))
	}
	task, err = waitForBackgroundTask(ctx, environment, "solidified")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["rootDigest"]) != manifestDigest || task["rootPublished"] != true || testkit.String(task["errorClass"]) != "" {
		return nil, fmt.Errorf("demand-triggered background task did not finish cleanly: status=%q errorClass=%q rootPublished=%v", testkit.String(task["status"]), testkit.String(task["errorClass"]), task["rootPublished"])
	}
	lines, err = fixtureLines(ctx, environment, "resume-background", started)
	if err != nil {
		return nil, err
	}
	offsets := fixtureRangeOffsets(lines)
	if len(offsets) != 4 || !allOffset(offsets, 1<<20) || countFixtureStatus(lines, "status=503") != 3 {
		return nil, fmt.Errorf("client-triggered retry did not retain the same committed offset: ranges=%v 503s=%d", offsets, countFixtureStatus(lines, "status=503"))
	}
	return map[string]any{
		"reference": "resume-background", "taskID": testkit.String(task["id"]),
		"taskStatus": testkit.String(task["status"]), "taskAttempts": testkit.Int(task["attempt"]),
		"rootDigest": testkit.String(task["rootDigest"]), "manifestDigest": manifestDigest,
		"configDigest": root.Config.Digest, "layerDigest": digest, "layerSize": len(blob), "rangeOffsets": offsets,
		"status503": countFixtureStatus(lines, "status=503"), "transmittedBytes": fixtureSentBytes(lines),
	}, nil
}

func waitForBackgroundTask(ctx context.Context, environment testkit.CaseContext, expectedStatus string) (map[string]any, error) {
	query := url.Values{"authority": {"github.com"}, "repository": {upstreamRepository}, "digest": {"resume-background"}, "limit": {"100"}}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/tasks?"+query.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		var page map[string]any
		if _, err := support.DecodeJSON(response, 4<<20, &page); err != nil {
			return nil, err
		}
		if task := backgroundTask(testkit.Slice(page["items"])); task != nil {
			status := testkit.String(task["status"])
			if status == expectedStatus {
				return task, nil
			}
			if status == "failed" || status == "cancelled" {
				encoded, _ := json.Marshal(task)
				return nil, fmt.Errorf("background download task ended in %s: %s", status, encoded)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("background resumable task did not reach %s", expectedStatus)
		case <-time.After(time.Second):
		}
	}
}

func backgroundTask(items []any) map[string]any {
	for _, item := range items {
		task := testkit.Map(item)
		if testkit.String(task["reference"]) == "resume-background" {
			return task
		}
	}
	return nil
}

func allOffset(offsets []int64, expected int64) bool {
	for _, offset := range offsets {
		if offset != expected {
			return false
		}
	}
	return len(offsets) > 0
}
