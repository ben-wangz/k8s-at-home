package app

import (
	"context"
	"registry-hub/internal/inventory"
	"testing"
	"time"
)

func TestBackgroundSnapshotUsesSourceCheckFreshness(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx, now := context.Background(), time.Now().UTC()
	snapshot := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("freshness"), ObservedAt: now.Add(-3 * time.Minute), LastSourceCheckAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := inventory.Update(ctx, f.service.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error { *value = snapshot; return nil }); err != nil {
		t.Fatal(err)
	}
	direct, err := f.service.inventory.Snapshot(ctx, "", f.service.inventoryConfig().StaleAfter)
	if err != nil {
		t.Fatal(err)
	}
	background := f.service.backgroundStatus(ctx)
	if background.Snapshot == nil || background.Snapshot.Freshness != direct.Freshness || direct.Freshness.State != "fresh" {
		t.Fatalf("freshness differs: direct=%+v background=%+v", direct.Freshness, background.Snapshot)
	}
}

func TestScanFreshnessDistinguishesScheduledDataAndFailures(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"statistics_tasks", "storage_inventory", "history_tasks", "source_tasks"} {
		interval := scanMaximumInterval(kind)
		value := backgroundScanStatus{LastSuccessAt: now.Add(-interval + 20*time.Second), NextScanAt: now.Add(20 * time.Second)}
		applyScanFreshness(&value, kind, now)
		if value.Stale || value.ScheduleOverdue || value.AgeSeconds == nil || value.Failed {
			t.Fatalf("normal scheduled scan is unhealthy: %s %+v", kind, value)
		}
		value.LastSuccessAt = now.Add(-interval - 2*scanScheduleTolerance)
		value.ErrorAt = now.Add(-time.Second)
		// A future retry does not renew old evidence.
		value.NextScanAt = now.Add(time.Hour)
		applyScanFreshness(&value, kind, now)
		if !value.Stale || !value.Failed || value.ScheduleOverdue {
			t.Fatalf("retry concealed stale evidence or failure: %s %+v", kind, value)
		}
		value.NextScanAt = now.Add(-2 * scanScheduleTolerance)
		applyScanFreshness(&value, kind, now)
		if !value.ScheduleOverdue {
			t.Fatal("missed schedule was not reported")
		}
	}
}
