package imageinventory

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "image-inventory")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	repository, err := support.DetailString(environment.DependencyResults["registry-roundtrip"], "repository")
	if err != nil {
		return err
	}
	authority, remote, _ := strings.Cut(repository, "/")
	client := testkit.NewHTTPClient(20*time.Second, nil)
	response, err := testkit.Request(ctx, client, http.MethodGet, environment.AdminURL+"/api/v1/images", nil, nil, nil)
	if err != nil {
		return err
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("inventory unauthenticated status=%d", response.StatusCode)
	}
	query := url.Values{"authority": {authority}, "repository": {remote}, "reference": {"minimal"}, "limit": {"1"}}
	var page map[string]any
	var image map[string]any
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		page, err = read(ctx, environment, "/api/v1/images?"+query.Encode())
		if err != nil {
			return err
		}
		items, _ := page["items"].([]any)
		if len(items) == 1 {
			image = testkit.Map(items[0])
			if testkit.String(image["category"]) == "solidified" {
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if image == nil || testkit.String(image["category"]) != "solidified" {
		return fmt.Errorf("pushed image not discovered as solidified: %v", image)
	}
	id := testkit.String(image["id"])
	if id == "" || testkit.String(testkit.Map(image["availability"])["state"]) != "complete" {
		return fmt.Errorf("incomplete inventory identity or availability")
	}
	image, snapshot, statusBefore, metricsBefore, err := awaitStableInventoryBaseline(ctx, environment, query, id)
	if err != nil {
		return err
	}
	if metricsBefore.snapshotWrites <= 0 || metricsBefore.snapshotWriteBytes <= 0 || metricsBefore.scanRounds <= 0 || metricsBefore.storageOperations <= 0 {
		return fmt.Errorf("inventory metrics do not reflect the initial registry scan and snapshot publication: %+v", metricsBefore)
	}
	refreshEvidence, err := awaitUnchangedRefresh(ctx, environment, query, snapshot, statusBefore, metricsBefore)
	if err != nil {
		return err
	}
	platformQuery := url.Values{"scope": {"platform"}, "authority": {authority}, "repository": {remote}, "digest": {testkit.String(image["resolvedDigest"])}, "snapshotID": {snapshot}}
	platforms, err := read(ctx, environment, "/api/v1/images?"+platformQuery.Encode())
	if err != nil {
		return err
	}
	platformItems, _ := platforms["items"].([]any)
	if len(platformItems) != 1 {
		return fmt.Errorf("resolved root has %d platform members, expected one", len(platformItems))
	}
	for _, path := range []string{"/api/v1/images/" + id, "/api/v1/images/" + id + "/platforms", "/api/v1/images/summary", "/api/v1/cache/objects"} {
		result, err := read(ctx, environment, path+"?snapshotID="+snapshot)
		if err != nil {
			return err
		}
		if testkit.String(result["snapshotID"]) != snapshot {
			return fmt.Errorf("snapshot changed for %s", path)
		}
	}
	objects, err := read(ctx, environment, "/api/v1/images/"+id+"/objects?snapshotID="+snapshot+"&limit=1")
	if err != nil {
		return err
	}
	count := 0
	for {
		items, _ := objects["items"].([]any)
		count += len(items)
		token := testkit.String(objects["nextToken"])
		if token == "" {
			break
		}
		objects, err = read(ctx, environment, "/api/v1/images/"+id+"/objects?limit=1&token="+url.QueryEscape(token))
		if err != nil {
			return err
		}
		if testkit.String(objects["snapshotID"]) != snapshot {
			return fmt.Errorf("object pagination switched snapshot")
		}
	}
	if count != 3 {
		return fmt.Errorf("closure has %d objects, expected manifest/config/layer", count)
	}
	for _, path := range []string{
		"/api/v1/images?category=invalid", "/api/v1/images?limit=1001", "/api/v1/cache/objects?cleanupPending=invalid",
		"/api/v1/images?category=solidified%7Ccached_not_started", "/api/v1/images?executionState=queued%7Crunning",
		"/api/v1/cache/objects?cacheState=complete%7Cpartial",
	} {
		response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			return fmt.Errorf("invalid inventory request returned %d", response.StatusCode)
		}
	}
	return support.WriteResult(environment, map[string]any{"snapshotID": snapshot, "imageID": id, "objects": count, "category": "solidified", "unchangedRefresh": refreshEvidence})
}

func awaitStableInventoryBaseline(ctx context.Context, environment testkit.CaseContext, query url.Values, imageID string) (map[string]any, string, inventoryStatus, inventoryMetrics, error) {
	deadline := time.Now().Add(2 * time.Minute)
	var previousStatus inventoryStatus
	var previousSnapshot string
	var missingMetrics []string
	stable := false
	for time.Now().Before(deadline) {
		status, err := readInventoryStatus(ctx, environment)
		if err != nil {
			return nil, "", inventoryStatus{}, inventoryMetrics{}, err
		}
		if !status.Building {
			page, err := read(ctx, environment, "/api/v1/images?"+query.Encode())
			if err != nil {
				return nil, "", inventoryStatus{}, inventoryMetrics{}, err
			}
			items, _ := page["items"].([]any)
			var currentImage map[string]any
			for _, item := range items {
				candidate := testkit.Map(item)
				if testkit.String(candidate["id"]) == imageID {
					currentImage = candidate
					break
				}
			}
			snapshot := testkit.String(page["snapshotID"])
			if currentImage != nil && snapshot != "" && testkit.String(currentImage["category"]) == "solidified" {
				if stable && snapshot == previousSnapshot && status.LastSourceCheck.Equal(previousStatus.LastSourceCheck) && status.LastPublishedAt.Equal(previousStatus.LastPublishedAt) {
					metrics, err := readInventoryMetrics(ctx, environment)
					if err != nil {
						return nil, "", inventoryStatus{}, inventoryMetrics{}, err
					}
					missingMetrics = metrics.initialBaselineMissing()
					if len(missingMetrics) == 0 {
						return currentImage, snapshot, status, metrics, nil
					}
				}
				if !stable || snapshot != previousSnapshot || !status.LastSourceCheck.Equal(previousStatus.LastSourceCheck) || !status.LastPublishedAt.Equal(previousStatus.LastPublishedAt) {
					previousStatus, previousSnapshot, stable = status, snapshot, true
					missingMetrics = nil
				}
			} else {
				stable = false
				missingMetrics = nil
			}
		} else {
			stable = false
			missingMetrics = nil
		}
		select {
		case <-ctx.Done():
			return nil, "", inventoryStatus{}, inventoryMetrics{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if len(missingMetrics) > 0 {
		return nil, "", inventoryStatus{}, inventoryMetrics{}, fmt.Errorf("stable inventory snapshot is missing initial scan metrics within two minutes: %s", strings.Join(missingMetrics, ", "))
	}
	return nil, "", inventoryStatus{}, inventoryMetrics{}, fmt.Errorf("inventory snapshot did not stabilize within two minutes")
}

type inventoryStatus struct {
	Building        bool      `json:"building"`
	LastSourceCheck time.Time `json:"lastSourceCheckAt"`
	LastPublishedAt time.Time `json:"lastPublishedAt"`
}

type inventoryMetrics struct {
	registryAttempts          float64
	registryAttemptsObserved  bool
	scanRounds                float64
	scanRoundsObserved        bool
	snapshotWrites            float64
	snapshotWritesObserved    bool
	snapshotWriteBytes        float64
	snapshotBytesObserved     bool
	storageOperations         float64
	storageOperationsObserved bool
	s3Attempts                float64
	s3AttemptsObserved        bool
}

func (m inventoryMetrics) initialBaselineMissing() []string {
	missing := []string{}
	if !m.registryAttemptsObserved || m.registryAttempts <= 0 {
		missing = append(missing, "registry_builder_attempts")
	}
	if !m.scanRoundsObserved || m.scanRounds <= 0 {
		missing = append(missing, "inventory_scan_rounds")
	}
	if !m.snapshotWritesObserved || m.snapshotWrites <= 0 {
		missing = append(missing, "snapshot_object_writes")
	}
	if !m.snapshotBytesObserved || m.snapshotWriteBytes <= 0 {
		missing = append(missing, "snapshot_object_write_bytes")
	}
	if !m.storageOperationsObserved || m.storageOperations <= 0 {
		missing = append(missing, "inventory_storage_operations")
	}
	return missing
}

func readInventoryStatus(ctx context.Context, environment testkit.CaseContext) (inventoryStatus, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/images/status", nil, nil)
	if err != nil {
		return inventoryStatus{}, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return inventoryStatus{}, fmt.Errorf("inventory status returned %s", response.Status)
	}
	var status inventoryStatus
	_, err = support.DecodeJSON(response, 1<<20, &status)
	if err != nil {
		return inventoryStatus{}, err
	}
	if status.LastSourceCheck.IsZero() || status.LastPublishedAt.IsZero() {
		return inventoryStatus{}, fmt.Errorf("inventory status is missing source-check or publication time")
	}
	return status, nil
}

func readInventoryMetrics(ctx context.Context, environment testkit.CaseContext) (inventoryMetrics, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return inventoryMetrics{}, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return inventoryMetrics{}, fmt.Errorf("metrics returned %s", response.Status)
	}
	metrics, err := testkit.ReadBody(response, 8<<20)
	if err != nil {
		return inventoryMetrics{}, err
	}
	var result inventoryMetrics
	for _, target := range []struct {
		name   string
		labels map[string]string
		value  *float64
		seen   *bool
	}{
		{name: "registry_hub_background_scan_rounds_total", labels: map[string]string{"kind": "inventory"}, value: &result.scanRounds, seen: &result.scanRoundsObserved},
		{name: "registry_hub_registry_http_attempts_total", labels: map[string]string{"kind": "inventory_builder"}, value: &result.registryAttempts, seen: &result.registryAttemptsObserved},
		{name: "registry_hub_inventory_snapshot_object_writes_total", labels: map[string]string{"kind": "inventory_builder", "purpose": "work", "reason": "inventory_snapshot_build"}, value: &result.snapshotWrites, seen: &result.snapshotWritesObserved},
		{name: "registry_hub_inventory_snapshot_object_write_bytes_total", labels: map[string]string{"kind": "inventory_builder", "purpose": "work", "reason": "inventory_snapshot_build"}, value: &result.snapshotWriteBytes, seen: &result.snapshotBytesObserved},
		{name: "registry_hub_storage_work_operations_total", labels: map[string]string{"kind": "inventory_builder"}, value: &result.storageOperations, seen: &result.storageOperationsObserved},
	} {
		total, found, err := testkit.PrometheusSum(metrics, target.name, target.labels)
		if err != nil {
			return inventoryMetrics{}, err
		}
		if !found {
			continue
		}
		*target.value = total
		*target.seen = true
	}
	result.s3Attempts, result.s3AttemptsObserved, err = testkit.PrometheusSum(metrics, "registry_hub_s3_http_attempts_total", map[string]string{"domain": "state", "kind": "inventory_builder"})
	if err != nil {
		return inventoryMetrics{}, err
	}
	return result, nil
}

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
			storageDelta := afterMetrics.storageOperations - baselineMetrics.storageOperations
			s3Delta := afterMetrics.s3Attempts - baselineMetrics.s3Attempts
			if storageDelta < 0 || storageDelta > 256 {
				return nil, fmt.Errorf("unchanged source refresh used %.0f inventory storage operations; budget is 0..256", storageDelta)
			}
			if s3Delta < 0 || s3Delta > 512 {
				return nil, fmt.Errorf("unchanged source refresh used %.0f inventory S3 HTTP attempts; budget is 0..512", s3Delta)
			}
			return map[string]any{
				"sourceCheckAt":                 status.LastSourceCheck,
				"storageWorkOperations":         storageDelta,
				"s3HTTPAttempts":                s3Delta,
				"s3HTTPAttemptsObserved":        afterMetrics.s3AttemptsObserved,
				"snapshotIDPreserved":           baselineSnapshot,
				"replacementPublicationsBefore": replacementPublications,
				"immutableSnapshotWrites":       afterMetrics.snapshotWrites - baselineMetrics.snapshotWrites,
				"registryHTTPAttemptsAdded":     afterMetrics.registryAttempts - baselineMetrics.registryAttempts,
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

func read(ctx context.Context, environment testkit.CaseContext, path string) (map[string]any, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("inventory %s returned %s", path, response.Status)
	}
	var result map[string]any
	_, err = support.DecodeJSON(response, 4<<20, &result)
	return result, err
}
