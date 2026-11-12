package app

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestStorageAttributionBoundsWorkerAndPurposeLabels(t *testing.T) {
	m := newMetrics()
	for i := 0; i < 100; i++ {
		ctx := context.WithValue(context.Background(), diagnosticContextKey{}, backgroundDiagnosticContext{Kind: fmt.Sprintf("unknown-%d", i), Scope: fmt.Sprintf("scope-%d", i)})
		m.recordStorageWork(ctx, "get", "success", time.Millisecond)
	}
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "registry_hub_storage_work_operations_total" {
			continue
		}
		if len(family.Metric) != 1 || family.Metric[0].GetCounter().GetValue() != 100 {
			t.Fatalf("unbounded attribution: %v", family)
		}
	}
	ctx := context.WithValue(context.Background(), diagnosticContextKey{}, backgroundDiagnosticContext{Kind: "quota_wake", Scope: "checkpoint"})
	if kind, purpose := storageWorkLabels(ctx); kind != "quota_wake" || purpose != "checkpoint" {
		t.Fatalf("labels=%s/%s", kind, purpose)
	}
	ctx = context.WithValue(ctx, storagePurposeKey{}, "queue_poll")
	if _, purpose := storageWorkLabels(ctx); purpose != "queue_poll" {
		t.Fatalf("purpose=%s", purpose)
	}
}
