package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

type storageStatusGuard struct {
	*inventoryWriteGuard
	gets, lists int
	fail        string
}

func (s *storageStatusGuard) Get(ctx context.Context, key string) (storage.Object, error) {
	s.gets++
	if key == s.fail {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func (s *storageStatusGuard) List(context.Context, string, string, int) ([]storage.Entry, string, error) {
	s.lists++
	return nil, "", storage.ErrPermissionDenied
}

func TestStorageStatusIndependentBoundedAndReadOnly(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	guard := &storageStatusGuard{inventoryWriteGuard: &inventoryWriteGuard{Store: f.service.store}}
	f.service.store = guard
	read := func(want string) storageCollectionStatus {
		t.Helper()
		guard.gets = 0
		response := diagnosticGet(t, f.service, "/api/v1/storage/status")
		var result storageCollectionStatus
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Phase != want || guard.gets != 3 || guard.lists != 0 || guard.writes != 0 {
			t.Fatalf("status: %d %s gets=%d lists=%d writes=%d", response.Code, response.Body.String(), guard.gets, guard.lists, guard.writes)
		}
		for _, private := range []string{"pendingRows", "sourceEntries", "private-token", "categories", "private-key"} {
			if strings.Contains(response.Body.String(), private) {
				t.Fatalf("private checkpoint field leaked: %s", private)
			}
		}
		if result.TotalObjects != nil || result.TotalBytes != nil || result.PercentComplete != nil {
			t.Fatal("unknown totals fabricated")
		}
		return result
	}
	read("not_observed")
	ctx := context.Background()
	state := newStorageCollection()
	state.Token, state.SourceNext = "private-token", "private-key"
	state.PendingRows = []storageObject{{ID: "private-key"}}
	state.Snapshot.StartedAt = time.Now().Add(-time.Hour)
	state.CheckpointAt, state.ProgressAt = time.Now().UTC(), time.Now().UTC()
	for _, row := range []struct {
		attribution, domain int
		merge, phase        string
	}{{0, 0, "", "attribution"}, {3, 0, "", "collect_state"}, {3, 1, "", "collect_registry"}, {3, 2, "reduce", "usage_reduce"}, {3, 2, "sort", "usage_sort"}, {3, 2, "done", "publishing"}} {
		state.AttributionPhase, state.Domain, state.UsageMerge.Phase = row.attribution, row.domain, row.merge
		if err := inventory.Update(ctx, guard.Store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil }); err != nil {
			t.Fatal(err)
		}
		result := read(row.phase)
		if result.ElapsedSeconds < 3599 || !result.CheckpointAt.Equal(state.CheckpointAt) {
			t.Fatal("persisted timing lost")
		}
		if row.phase == "attribution" && result.Domains["registry"].State != "waiting" {
			t.Fatal("unstarted domain is not waiting")
		}
	}
	expired := storageSnapshot{SnapshotID: inventory.ID("old"), CompletedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	if err := inventory.Update(ctx, guard.Store, storageInventoryPrefix+"current.json", func(value *storageSnapshot) error { *value = expired; return nil }); err != nil {
		t.Fatal(err)
	}
	result := read("publishing")
	if result.PreviousReport == nil || result.PreviousReport.Available || result.PreviousReport.Freshness != "expired" {
		t.Fatal("expired report status lost")
	}
	guard.fail = storageInventoryPrefix + "checkpoint.json"
	result = read("unknown")
	if result.ReadErrors["collection"] == "" {
		t.Fatal("failed source not disclosed")
	}
	guard.fail = ""
	f.service.cfg.AdminAuth.Basic[0].Permissions = []string{"task-admin"}
	read("publishing")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/storage/status", nil)
	response := httptest.NewRecorder()
	f.service.AdminHandler().ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("unauthenticated status allowed")
	}
}

func TestStorageStatusFailureAndLegacyCheckpoint(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	guard := &storageStatusGuard{inventoryWriteGuard: &inventoryWriteGuard{Store: f.service.store}, fail: storageInventoryPrefix + "checkpoint.json"}
	f.service.store = guard
	if _, err := f.service.storageCollectionStatus(context.Background()); err == nil {
		t.Fatal("unreadable essential state reported as never observed")
	}
	guard.fail = ""
	state := newStorageCollection()
	state.EntriesExamined = nil
	state.AttributionObjects = nil
	objects, size := int64(7), int64(90)
	domain := state.Snapshot.Domains["state"]
	domain.Objects, domain.ObjectBytes = &objects, &size
	state.Snapshot.Domains["state"] = domain
	if err := inventory.Update(context.Background(), guard.Store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil }); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.storageCollectionStatus(context.Background())
	if err != nil || result.EntriesExamined != nil || result.ObjectsCommitted == nil || *result.ObjectsCommitted != 7 || result.CounterReason == "" {
		t.Fatalf("legacy=%+v err=%v", result, err)
	}
}

func TestStorageStatusRetainsRetryHistoryAndDomainCoverage(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	now := time.Now().UTC()
	state := newStorageCollection()
	state.AttributionPhase, state.Domain = 3, 2
	objects, size := int64(7), int64(90)
	state.Snapshot.Domains["state"] = storageDomainSummary{Domain: "state", State: "partial", Objects: &objects, ObjectBytes: &size, Reason: "timeout", Coverage: []string{"committed lower bounds"}}
	state.Snapshot.Domains["registry"] = storageDomainSummary{Domain: "registry", State: "unavailable", Reason: "inspection_not_configured"}
	if err := inventory.Update(ctx, f.service.store, storageInventoryPrefix+"checkpoint.json", func(value *storageCollection) error { *value = state; return nil }); err != nil {
		t.Fatal(err)
	}
	maintenance := maintenanceCheckpoint{Status: backgroundScanStatus{StartedAt: now.Add(-time.Hour), ErrorAt: now.Add(-time.Minute), ErrorClass: "timeout", NextAttemptAt: now.Add(time.Minute), Processed: 12}}
	save := func() {
		t.Helper()
		if err := inventory.Update(ctx, f.service.store, maintenanceCheckpointKey("storage_inventory"), func(value *maintenanceCheckpoint) error { *value = maintenance; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	save()
	status, err := f.service.storageCollectionStatus(ctx)
	if err != nil || !status.Backoff || !status.NextRetryAt.Equal(maintenance.Status.NextAttemptAt) || status.ErrorClass != "timeout" || !status.ErrorAt.Equal(maintenance.Status.ErrorAt) || status.ExecutionUnits != 12 {
		t.Fatalf("retry evidence=%+v err=%v", status, err)
	}
	if status.Domains["state"].State != "partial" || *status.Domains["state"].ObjectBytes != size || status.Domains["registry"].State != "unavailable" || status.Domains["registry"].Objects != nil || status.TotalBytes != nil || status.PercentComplete != nil {
		t.Fatalf("domain uncertainty lost: %+v", status)
	}
	maintenance.Status.ErrorRecovered = true
	maintenance.Status.LastSuccessAt = now
	maintenance.Status.NextAttemptAt = time.Time{}
	save()
	status, err = f.service.storageCollectionStatus(ctx)
	if err != nil || status.Backoff || status.ErrorClass != "timeout" || !status.ErrorAt.Equal(maintenance.Status.ErrorAt) || !status.RecoveredAt.Equal(now) {
		t.Fatalf("recovery erased failure evidence: %+v err=%v", status, err)
	}
}
