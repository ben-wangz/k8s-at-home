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
