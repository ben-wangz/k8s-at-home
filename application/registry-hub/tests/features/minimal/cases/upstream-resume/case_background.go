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
	task, err := waitForBackgroundTask(ctx, environment)
	if err != nil {
		return nil, err
	}
	expected, digest := buildResumeLayer("background")
	blob, err := support.ReadBlob(ctx, registry, "github.com/"+upstreamRepository, digest)
	if err != nil || len(blob) != len(expected) || support.Digest(blob) != digest {
		return nil, fmt.Errorf("solidified background layer is invalid: err=%v size=%d", err, len(blob))
	}
	lines, err := fixtureLines(ctx, environment, "resume-background", started)
	if err != nil {
		return nil, err
	}
	offsets := fixtureRangeOffsets(lines)
	if len(offsets) < 4 || !allOffset(offsets, 1<<20) || countFixtureStatus(lines, "status=503") != 3 {
		return nil, fmt.Errorf("background retry did not retain the same committed offset: ranges=%v 503s=%d", offsets, countFixtureStatus(lines, "status=503"))
	}
	return map[string]any{
		"reference": "resume-background", "taskID": testkit.String(task["id"]),
		"taskStatus": testkit.String(task["status"]), "taskAttempts": testkit.Int(task["attempt"]),
		"rootDigest": testkit.String(task["rootDigest"]), "manifestDigest": manifestDigest,
		"layerDigest": digest, "layerSize": len(blob), "rangeOffsets": offsets,
		"status503": countFixtureStatus(lines, "status=503"), "transmittedBytes": fixtureSentBytes(lines),
	}, nil
}

func waitForBackgroundTask(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
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
			if status == "solidified" && task["rootPublished"] == true {
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
			return nil, fmt.Errorf("background resumable download did not solidify")
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
