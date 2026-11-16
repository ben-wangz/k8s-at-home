package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"registry-hub/internal/inventory"
	"strings"
	"testing"
	"time"
)

func TestStorageCollectorAdoptsOversizedLegacyPendingPage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	owner, err := f.service.tasks.Leases().Acquire(ctx, "diagnostics/storage-inventory", "legacy-adoption", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, owner)
	state := newStorageCollection()
	state.AttributionPhase, state.Pending = 3, true
	state.EntriesExamined, state.AttributionObjects, state.ListPageSize = nil, nil, 0
	for n := range 4 {
		state.PendingRows = append(state.PendingRows, storageObject{
			ID: fmt.Sprintf("legacy-%d", n), PhysicalID: fmt.Sprintf("physical-%d", n),
			Domain: "state", Category: "unclassified", ObjectBytes: 7,
			ObservedAt:     time.Unix(1700000000+int64(n), 0).UTC(),
			EvidenceSource: strings.Repeat("legacy-evidence;", 80000),
		})
	}
	expected, err := json.Marshal(state.PendingRows)
	if err != nil || len(expected) <= 4<<20 || len(expected) >= 16<<20 {
		t.Fatalf("legacy fixture must exceed new bounds within old bounds: %d bytes, %v", len(expected), err)
	}
	if err := f.service.checkpointStorageCollection(ctx, owner, &state); err != nil {
		t.Fatal(err)
	}
	state, _, err = inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshotID := state.Snapshot.SnapshotID
	if _, err := f.service.storageCollectionQuantum(ctx, owner, &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	actual, _, err := inventory.Read[json.RawMessage](ctx, f.service.store, storagePageKey(snapshotID, 0))
	if err != nil || !bytes.Equal(expected, actual) {
		t.Fatalf("legacy output changed during adoption: %v", err)
	}
	state, _, err = inventory.Read[storageCollection](ctx, f.service.store, storageInventoryPrefix+"checkpoint.json")
	if err != nil || state.Pending || state.SourceListed || state.Domain != 1 || state.Snapshot.Pages != 1 {
		t.Fatalf("legacy continuation was not adopted: domain=%d pages=%d pending=%t error=%v", state.Domain, state.Snapshot.Pages, state.Pending, err)
	}
	summary := state.Snapshot.Domains["state"]
	if summary.Objects == nil || *summary.Objects != 4 || summary.ObjectBytes == nil || *summary.ObjectBytes != 28 {
		t.Fatalf("legacy page was lost or counted twice: %+v", summary)
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > 4<<20 {
		t.Fatalf("adopted checkpoint exceeds new bound: %d bytes, %v", len(encoded), err)
	}
}

func TestStorageCollectorKeepsLegacyCoverageAcrossResume(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	key := storageInventoryPrefix + "snapshots/" + inventory.ID("legacy-history") + "/objects/00000000.json"
	if err := inventory.Update(ctx, f.service.store, key, func(value *string) error { *value = "historical output"; return nil }); err != nil {
		t.Fatal(err)
	}
	state := newStorageCollection()
	domain := state.Snapshot.Domains["state"]
	domain.SummaryOnlyCategories = nil
	domain.Coverage = domain.Coverage[:len(domain.Coverage)-1]
	state.Snapshot.Domains["state"] = domain
	for steps := 0; state.NextScanAt.IsZero(); steps++ {
		if steps > 1000 {
			t.Fatal("legacy collection did not finish")
		}
		if err := f.service.advanceStorageCollection(ctx, &state, nil, nil); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		state = storageCollection{}
		if err := json.Unmarshal(encoded, &state); err != nil {
			t.Fatal(err)
		}
	}
	report, err := f.service.storageSnapshot(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Domains["state"].SummaryOnlyCategories) != 0 || report.Cost != (storageCollectionCost{}) {
		t.Fatal("legacy collection changed its coverage or advertised partial cost")
	}
	var found bool
	for page := range report.Pages {
		rows, _, err := inventory.Read[[]storageObject](ctx, f.service.store, storagePageKey(report.SnapshotID, page))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			found = found || row.Category == "storage_inventory_output"
		}
	}
	if !found {
		t.Fatal("legacy internal output detail disappeared during resume")
	}
	manifest := storageInventoryPrefix + "snapshots/" + report.SnapshotID + "/manifest.json"
	data, version, err := inventory.Read[json.RawMessage](ctx, f.service.store, manifest)
	if err != nil || bytes.Contains(data, []byte(`"cost"`)) {
		t.Fatalf("legacy manifest gained unsupported cost fields: %s %v", data, err)
	}
	state = storageCollection{Snapshot: report, Domain: 2, AttributionPhase: 3, UsageMerge: storageUsageMerge{Phase: "done"}}
	if err := f.service.publishStorageCollection(ctx, &state); err != nil {
		t.Fatalf("unknown legacy publication could not replay: %v", err)
	}
	entry, err := f.service.store.Head(ctx, manifest)
	if err != nil || entry.Version != version {
		t.Fatalf("replay rewrote legacy manifest: %+v %v", entry, err)
	}
}
