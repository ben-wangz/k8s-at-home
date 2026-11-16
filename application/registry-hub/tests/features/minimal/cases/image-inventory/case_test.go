package imageinventory

import (
	"strings"
	"testing"
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
