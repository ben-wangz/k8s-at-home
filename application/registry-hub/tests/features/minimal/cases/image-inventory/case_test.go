package imageinventory

import (
	"strings"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestInventoryMetricsWaitForInitialProcessSamples(t *testing.T) {
	metrics := inventoryMetrics{}
	if got, want := strings.Join(metrics.initialBaselineMissing(), ","), "registry_builder_attempts,inventory_scan_rounds,snapshot_object_writes,snapshot_object_write_bytes,inventory_storage_operations"; got != want {
		t.Fatalf("missing initial metrics = %q, want %q", got, want)
	}

	metrics = inventoryMetrics{
		registryAttempts: 1, registryAttemptsObserved: true,
		scanRounds: 1, scanRoundsObserved: true,
		snapshotWrites: 1, snapshotWritesObserved: true,
		snapshotWriteBytes: 1, snapshotBytesObserved: true,
		storageOperations: 1, storageOperationsObserved: true,
	}
	if got := metrics.initialBaselineMissing(); len(got) != 0 {
		t.Fatalf("complete initial metrics remain missing: %v", got)
	}

	metrics.scanRounds = 0
	if got := strings.Join(metrics.initialBaselineMissing(), ","); got != "inventory_scan_rounds" {
		t.Fatalf("zero-valued initial scan metric was accepted: %q", got)
	}
}

func TestStorageWorkDeltaSortsAndOmitsUnchangedSeries(t *testing.T) {
	before := map[string]float64{"operation=get": 3, "operation=list": 2}
	after := map[string]float64{"operation=get": 5, "operation=list": 2, "operation=put": 1}
	got := strings.Join(storageWorkDelta(before, after), ";")
	if want := "operation=get delta=2;operation=put delta=1"; got != want {
		t.Fatalf("storageWorkDelta() = %q, want %q", got, want)
	}
}

func TestInventoryStorageWorkSeparatesRefreshFromMaintenance(t *testing.T) {
	metrics := inventoryMetrics{
		storageWorkByLabel:        map[string]float64{},
		refreshStorageWorkByLabel: map[string]float64{},
	}
	addInventoryStorageWorkSample(&metrics, testkit.PrometheusSample{
		Labels: map[string]string{"kind": "inventory_builder", "operation": "list", "key_class": "inventory", "reason": "inventory_maintenance", "result": "success"}, Value: 195,
	})
	addInventoryStorageWorkSample(&metrics, testkit.PrometheusSample{
		Labels: map[string]string{"kind": "inventory_builder", "operation": "get", "key_class": "inventory", "reason": "inventory_refresh", "result": "success"}, Value: 8,
	})

	if metrics.storageOperations != 203 || metrics.refreshStorageOperations != 8 || !metrics.refreshStorageOperationsObserved {
		t.Fatalf("storage totals = all %.0f, refresh %.0f, observed %t; want 203, 8, true", metrics.storageOperations, metrics.refreshStorageOperations, metrics.refreshStorageOperationsObserved)
	}
}
