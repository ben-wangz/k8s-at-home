package app

import (
	"bytes"
	"context"
	"fmt"
	"registry-hub/internal/distribution"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestRegistryHTTPAttemptMetricsKeepBoundedWorkAttribution(t *testing.T) {
	m := newMetrics()
	ctx := context.WithValue(context.Background(), diagnosticContextKey{}, backgroundDiagnosticContext{Kind: "inventory_builder", Scope: "execution"})
	ctx = withStorageReason(ctx, "inventory_snapshot_build")
	m.recordRegistryHTTPAttempt(distribution.HTTPAttempt{Context: ctx, Method: "HEAD", Operation: "manifests", StatusCode: 200})
	assertMetricsCounter(t, m, "registry_hub_registry_http_attempts_total", map[string]string{
		"kind": "inventory_builder", "purpose": "work", "method": "HEAD", "operation": "manifests", "reason": "inventory_snapshot_build", "result": "success",
	}, 1)

	for n := 0; n < 20; n++ {
		unknown := context.WithValue(context.Background(), diagnosticContextKey{}, backgroundDiagnosticContext{Kind: fmt.Sprintf("unknown-%d", n)})
		m.recordRegistryHTTPAttempt(distribution.HTTPAttempt{Context: unknown, Method: "HEAD", Operation: "manifests", StatusCode: 200})
	}
	assertMetricsCounter(t, m, "registry_hub_registry_http_attempts_total", map[string]string{
		"kind": "other", "purpose": "work", "method": "HEAD", "operation": "manifests", "reason": "unspecified", "result": "success",
	}, 20)
}

func TestBackgroundScanRoundMetricsDistinguishResultAndBoundKind(t *testing.T) {
	m := newMetrics()
	m.recordBackgroundScanRound("source_tasks", true)
	m.recordBackgroundScanRound("source_tasks", false)
	for n := 0; n < 20; n++ {
		m.recordBackgroundScanRound(fmt.Sprintf("unknown-%d", n), true)
	}
	assertMetricsCounter(t, m, "registry_hub_background_scan_rounds_total", map[string]string{"kind": "source_tasks", "result": "complete"}, 1)
	assertMetricsCounter(t, m, "registry_hub_background_scan_rounds_total", map[string]string{"kind": "source_tasks", "result": "incomplete"}, 1)
	assertMetricsCounter(t, m, "registry_hub_background_scan_rounds_total", map[string]string{"kind": "other", "result": "complete"}, 20)
}

func TestInventorySnapshotMetricsCountOnlyCreatedSnapshotObjects(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	m := newMetrics()
	store := newObservedStore(base, m, nil)
	ctx := context.WithValue(context.Background(), diagnosticContextKey{}, backgroundDiagnosticContext{Kind: "inventory_builder", Scope: "execution"})
	ctx = withStorageReason(ctx, "inventory_snapshot_build")
	key := inventory.Prefix + "snapshots/test-snapshot/manifest.json"
	payload := []byte(`{"snapshot":"test-snapshot"}`)
	put := func(key string) error {
		_, err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload)), storage.PutOptions{IfAbsent: true})
		return err
	}
	if err := put(key); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{"kind": "inventory_builder", "purpose": "work", "reason": "inventory_snapshot_build"}
	assertMetricsCounter(t, m, "registry_hub_inventory_snapshot_object_writes_total", labels, 1)
	assertMetricsCounter(t, m, "registry_hub_inventory_snapshot_object_write_bytes_total", labels, float64(len(payload)))

	if err := put(key); err == nil {
		t.Fatal("duplicate immutable snapshot write unexpectedly succeeded")
	}
	if err := put(inventory.Prefix + "current.json"); err != nil {
		t.Fatal(err)
	}
	assertMetricsCounter(t, m, "registry_hub_inventory_snapshot_object_writes_total", labels, 1)
	assertMetricsCounter(t, m, "registry_hub_inventory_snapshot_object_write_bytes_total", labels, float64(len(payload)))
	if got := storageKeyClass(key); got != "inventory_snapshot" {
		t.Fatalf("snapshot key class=%q, want inventory_snapshot", got)
	}
}

func assertMetricsCounter(t *testing.T, m *metrics, name string, labels map[string]string, want float64) {
	t.Helper()
	value, ok := metricsCounterValue(t, m, name, labels)
	if !ok || value != want {
		t.Fatalf("%s labels=%v value=%v present=%t, want %v", name, labels, value, ok, want)
	}
}

func metricsCounterValue(t *testing.T, m *metrics, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			actual := make(map[string]string, len(metric.Label))
			for _, label := range metric.Label {
				actual[label.GetName()] = label.GetValue()
			}
			if len(actual) != len(labels) {
				continue
			}
			matches := true
			for key, value := range labels {
				if actual[key] != value {
					matches = false
					break
				}
			}
			if matches {
				return metric.GetCounter().GetValue(), true
			}
		}
	}
	return 0, false
}
