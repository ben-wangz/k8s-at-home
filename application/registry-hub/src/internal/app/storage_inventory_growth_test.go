package app

import (
	"context"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
)

func TestStorageReportsDoNotMaterializeHistoricalOutput(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	var history []storageSnapshot
	for round := range 10 {
		entries, _, err := storage.Scan(ctx, f.service.store, "", "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		var bytes int64
		for _, entry := range entries {
			bytes += entry.Size
		}
		report := collectTestStorage(t, f.service, nil)
		state := report.Domains["state"]
		if *state.Objects != int64(len(entries)) || *state.ObjectBytes != bytes ||
			!containsStorageCategory(state.SummaryOnlyCategories, "storage_inventory_output") ||
			!containsStorageCategory(state.SummaryOnlyCategories, "diagnostic_events") {
			t.Fatalf("round %d lost physical totals or coverage: %+v", round, state)
		}
		for page := range report.Pages {
			rows, _, err := inventory.Read[[]storageObject](ctx, f.service.store, storagePageKey(report.SnapshotID, page))
			if err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.Category == "storage_inventory_output" {
					t.Fatal("historical output generated an object row")
				}
			}
		}
		if report.Pages > 2 || report.Cost.OutputObjects > 20 || report.Cost.OutputBytes > 50000 {
			t.Fatalf("round %d amplified fixed input: pages=%d cost=%+v", round, report.Pages, report.Cost)
		}
		history = append(history, report)
		if round == 4 || round == 8 {
			id := history[round-4].SnapshotID
			manifest := storageInventoryPrefix + "snapshots/" + id + "/manifest.json"
			if err := inventory.Update(ctx, f.service.store, manifest, func(value *storageSnapshot) error { value.ExpiresAt = value.StartedAt.Add(-1); return nil }); err != nil {
				t.Fatal(err)
			}
			entry, err := f.service.store.Head(ctx, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.cleanupStorageObservation(ctx, entry); err != nil {
				t.Fatal(err)
			}
			for range 10 {
				if err := f.service.advanceStorageRetention(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			remaining, _, err := storage.Scan(ctx, f.service.store, storageInventoryPrefix+"snapshots/"+id+"/", "", 1000)
			if err != nil || len(remaining) != 0 {
				t.Fatalf("expired output survived: %d %v", len(remaining), err)
			}
		}
	}
}

func TestStorageDiagnosticEventSummaryUsesListMetadataOnly(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	objects := map[string]string{
		"diagnostics/v1/events/20261121/one.json":          `{"event":"one"}`,
		"diagnostics/v1/events/20261121/two.json":          `{"event":"two"}`,
		"diagnostics/v2/event-batches/20261121/batch.json": `{"events":[{"event":"three"}]}`,
		"diagnostics/v1/work-history/notify/one.json":      `{"history":"one"}`,
		"diagnostics/v1/transfer-history/one.json":         `{"history":"transfer-one"}`,
	}
	var expectedEventBytes int64
	var expectedEventObjects int64
	for key, body := range objects {
		if _, err := f.service.store.Put(ctx, key, strings.NewReader(body), int64(len(body)), storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(key, "diagnostics/v1/events/") || strings.HasPrefix(key, "diagnostics/v2/event-batches/") {
			expectedEventBytes += int64(len(body))
			expectedEventObjects++
		}
	}

	counter := &storageDiagnosticGetCounter{Store: f.service.store}
	f.service.store, f.service.inventory.Store = counter, counter
	report := collectTestStorage(t, f.service, nil)
	usage := report.Domains["state"].Categories["diagnostic_events"]
	if usage.Objects != expectedEventObjects || usage.ObjectBytes != expectedEventBytes {
		t.Fatalf("diagnostic event summary = %+v; want objects=%d bytes=%d", usage, expectedEventObjects, expectedEventBytes)
	}
	if counter.eventGets != 0 {
		t.Fatalf("diagnostic event GET calls = %d; want 0", counter.eventGets)
	}
	if counter.historyGets != 0 || report.Domains["state"].Categories["terminal_work_history"].Objects != 1 || report.Domains["state"].Categories["terminal_transfer_history"].Objects != 1 {
		t.Fatalf("history usage work=%+v transfer=%+v per-object GETs=%d; want one object per category and zero GETs", report.Domains["state"].Categories["terminal_work_history"], report.Domains["state"].Categories["terminal_transfer_history"], counter.historyGets)
	}
}

type storageDiagnosticGetCounter struct {
	storage.Store
	eventGets   int
	historyGets int
}

func (s *storageDiagnosticGetCounter) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, "diagnostics/v1/events/") || strings.HasPrefix(key, "diagnostics/v2/event-batches/") {
		s.eventGets++
	}
	if strings.HasPrefix(key, terminalWorkHistoryPrefix) || strings.HasPrefix(key, terminalTransferHistoryPrefix) {
		s.historyGets++
	}
	return s.Store.Get(ctx, key)
}

func containsStorageCategory(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStorageSummaryOnlyInputDoesNotCreateUsageRunsPerPage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	for n := range 1000 {
		key := fmt.Sprintf("%ssnapshots/%s/objects/%08d.json", storageInventoryPrefix, inventory.ID("history"), n)
		if err := inventory.Update(ctx, f.service.store, key, func(value *string) error { *value = "historical output"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	report := collectTestStorage(t, f.service, nil)
	if report.Cost.SummaryOnlyObjects != 1000 || report.Pages != 1 || report.Cost.OutputObjects > 20 {
		t.Fatalf("summary-only input amplified output: pages=%d cost=%+v", report.Pages, report.Cost)
	}
	category := report.Domains["state"].Categories["storage_inventory_output"]
	if category.Objects != 1000 {
		t.Fatalf("summary lost internal objects: %+v", category)
	}
	var found bool
	for page := range report.UsagePages {
		rows, _, err := inventory.Read[[]storageUsage](ctx, f.service.store, usageRunPage(report.SnapshotID, report.UsageRun, page))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Domain == "state" && row.Mask == 1 && row.Category == "storage_inventory_output" {
				found = row.Objects == category.Objects && row.ObjectBytes == category.ObjectBytes
			}
		}
	}
	if !found {
		t.Fatal("category usage does not include summary-only output")
	}
}
