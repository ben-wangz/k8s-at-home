package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"strings"
	"testing"
	"time"
)

func TestInventoryExpiredLatestIsUnavailableAndPinnedSnapshotsAreGone(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	initial := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("expired-complete"), BuildStartedAt: time.Now().Add(-2 * time.Hour).UTC(), ObservedAt: time.Now().Add(-2 * time.Hour).UTC(), ExpiresAt: time.Now().Add(-time.Minute), Coverage: "complete", UniqueManifests: 1}
	seedInventoryCurrent(t, f.service, initial)
	if err := f.service.inventory.Immutable(ctx, inventory.Prefix+"checkpoint.json", inventoryCheckpoint{NextScanAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	leader, err := f.service.tasks.Leases().Acquire(ctx, "inventory/builder", "expired", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, leader)
	leaderCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	if err := f.service.runInventoryLeader(leaderCtx, leader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("leader stopped before the idle deadline: %v", err)
	}
	current, _, err := inventory.Read[inventory.Snapshot](ctx, f.service.store, inventory.Prefix+"current.json")
	if err != nil || current.BuilderFence != leader.FencingToken || current.SnapshotID != initial.SnapshotID || !current.ObservedAt.Equal(initial.ObservedAt) || !current.ExpiresAt.Equal(initial.ExpiresAt) {
		t.Fatal("leader restart replaced or renewed a completed snapshot")
	}
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "operator", Password: "secret", Permissions: []string{auth.TaskAdmin}}}}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store, f.service.inventory.Store = guard, guard
	handler := f.service.AdminHandler()
	id := inventory.ID("image")
	paths := []string{"/api/v1/images", "/api/v1/images?scope=platform", "/api/v1/images/summary", "/api/v1/cache/objects", "/api/v1/images/" + id, "/api/v1/images/" + id + "/objects", "/api/v1/images/" + id + "/platforms"}
	for _, path := range paths {
		for _, pinned := range []bool{false, true} {
			target := path
			if pinned {
				separator := "?"
				if strings.Contains(target, "?") {
					separator = "&"
				}
				target += separator + "snapshotID=" + initial.SnapshotID
			}
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, target, nil)
			r.SetBasicAuth("operator", "secret")
			handler.ServeHTTP(w, r)
			want := http.StatusServiceUnavailable
			if pinned {
				want = http.StatusGone
			}
			if w.Code != want || !pinned && (w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), "/api/v1/images/status")) {
				t.Fatalf("GET %s: status=%d body=%s", target, w.Code, w.Body.String())
			}
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/images/status", nil)
	r.SetBasicAuth("operator", "secret")
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK || guard.writes != 0 {
		t.Fatalf("status=%d inventory GET writes=%d", w.Code, guard.writes)
	}
}
