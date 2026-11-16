package imageinventory

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

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
