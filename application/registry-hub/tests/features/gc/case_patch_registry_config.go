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

func patchRegistryConfig(ctx context.Context, environment testkit.CaseContext, value string) error {
	patch, err := json.Marshal(map[string]any{"data": map[string]string{"config.yml": value}})
	if err != nil {
		return err
	}
	_, err = testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "patch", "configmap", environment.Fullname+"-registry-config", "--type=merge", "-p", string(patch))
	return err
}

func verifyGCWriteAdmission(ctx context.Context, registry *support.RegistryClient, repository string) error {
	response, err := registry.Request(ctx, http.MethodPost, registry.Endpoint(repository, "/blobs/uploads/"), nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusServiceUnavailable || response.Header.Get("Retry-After") != "5" {
		response.Body.Close()
		return fmt.Errorf("write during GC returned %s with Retry-After %q", response.Status, response.Header.Get("Retry-After"))
	}
	var envelope struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	if _, err := support.DecodeJSON(response, 1<<20, &envelope); err != nil {
		return err
	}
	if len(envelope.Errors) != 1 || envelope.Errors[0].Code != "UNAVAILABLE" {
		return fmt.Errorf("write during GC returned an unexpected Registry error envelope")
	}
	return nil
}

func verifyGCReadAdmission(ctx context.Context, registry *support.RegistryClient, repository string, retained support.ImageFixture) error {
	manifest, headers, err := support.ReadManifest(ctx, registry, repository, retained.Tag, support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("Registry read failed while GC was draining: %w", err)
	}
	if !bytes.Equal(manifest, retained.Manifest) || !strings.EqualFold(headers.Get("Docker-Content-Digest"), retained.ManifestDigest) {
		return fmt.Errorf("Registry changed the retained manifest while GC was draining")
	}
	return nil
}

func verifyGCMetrics(ctx context.Context, environment testkit.CaseContext, wantSuccess, wantFailure, wantDuration int) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
		if err != nil {
			return err
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return fmt.Errorf("metrics returned %s", response.Status)
		}
		metrics, err := testkit.ReadBody(response, 4<<20)
		if err != nil {
			return err
		}
		success, failure, duration := false, false, false
		for _, line := range strings.Split(string(metrics), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if fields[0] == `registry_hub_gc_runs_total{result="success"}` && fields[1] == fmt.Sprint(wantSuccess) {
				success = true
			}
			if fields[0] == `registry_hub_gc_runs_total{result="failure"}` && fields[1] == fmt.Sprint(wantFailure) {
				failure = true
			}
			if fields[0] == "registry_hub_gc_duration_seconds_count" && fields[1] == fmt.Sprint(wantDuration) {
				duration = true
			}
		}
		if success && failure && duration {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("GC failure, success and duration metrics were not recorded")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func cleanupGCJob(ctx context.Context, environment testkit.CaseContext, name, expectedUID string) error {
	job, err := testkit.KubectlGet(ctx, environment.Namespace, "job", name)
	if err != nil {
		return err
	}
	if expectedUID == "" || testkit.String(testkit.Map(job["metadata"])["uid"]) != expectedUID {
		return fmt.Errorf("refusing to delete GC Job with an unexpected UID")
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "delete", "job", name, "--cascade=foreground", "--wait=true", "--timeout=60s"); err != nil {
		return fmt.Errorf("delete completed GC Job: %w", err)
	}
	if _, err := testkit.KubectlGet(ctx, environment.Namespace, "job", name); !testkit.IsNotFound(err) {
		return fmt.Errorf("completed GC Job still exists after cleanup")
	}
	return nil
}

func startGC(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusAccepted {
		response.Body.Close()
		return nil, fmt.Errorf("enabled GC trigger returned %s", response.Status)
	}
	var run map[string]any
	if _, err := support.DecodeJSON(response, 2<<20, &run); err != nil {
		return nil, err
	}
	if testkit.String(run["id"]) == "" || testkit.String(run["state"]) != "draining" {
		return nil, fmt.Errorf("GC trigger returned an incomplete run identity")
	}
	return run, nil
}

func retryGC(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/gc/retry", bytes.NewReader([]byte(`{"confirmedNoExecutor":true}`)), map[string]string{"Content-Type": "application/json"})
		if err != nil {
			return nil, err
		}
		if response.StatusCode == http.StatusAccepted {
			var run map[string]any
			if _, err := support.DecodeJSON(response, 2<<20, &run); err != nil {
				return nil, err
			}
			if testkit.String(run["id"]) == "" || testkit.String(run["state"]) != "draining" {
				return nil, fmt.Errorf("GC retry returned an incomplete run identity")
			}
			return run, nil
		}
		status := response.StatusCode
		response.Body.Close()
		if status != http.StatusConflict || time.Now().After(deadline) {
			return nil, fmt.Errorf("GC retry returned unexpected status %d", status)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
