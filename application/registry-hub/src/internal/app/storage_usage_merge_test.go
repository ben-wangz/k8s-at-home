package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"slices"
	"testing"
	"time"
)

func TestStorageUsageExceedsLegacyCapWithBoundedReplayAndPagination(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	verifyStorageUsageMergeAndPagination(t, context.Background(), f)
}

func TestStorageUsageMergeAgainstS3(t *testing.T) {
	remote := newDedicatedS3AcceptanceStore(t, "REGISTRY_HUB_TEST_STORAGE_USAGE_S3_CONFIG")
	store := &inventoryLatencyStore{Store: remote}
	f := newStagedAcceptanceFixtureWithStore(t, 1<<20, store)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	started, calls := time.Now(), store.calls.Load()
	verifyStorageUsageMergeAndPagination(t, ctx, f)
	if time.Since(started) > 15*time.Minute {
		t.Fatalf("4,227-group merge and paginated API exceeded 15 minutes: elapsed=%s calls=%d", time.Since(started), store.calls.Load()-calls)
	}
	t.Logf("4,227 groups, 1,056 image owners: elapsed=%s calls=%d", time.Since(started), store.calls.Load()-calls)
}

func verifyStorageUsageMergeAndPagination(t *testing.T, ctx context.Context, f *stagedAcceptanceFixture) {
	t.Helper()
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	state := newStorageCollection()
	shared := true
	var expectedBytes int64
	for n := 0; n < 66; n++ {
		object := storageObject{Domain: "state", Category: "cache_payloads", ObjectBytes: int64(n + 1), Shared: &shared}
		for image := 0; image < 16; image++ {
			object.Associations = append(object.Associations, storageAssociation{Authority: "example.org", Repository: "team/image", ImageID: fmt.Sprintf("image-%04d", n*16+image)})
		}
		addStorageUsage(&state, object)
		expectedBytes += object.ObjectBytes
		if n%4 == 3 || n == 65 {
			if err := f.service.flushStorageUsage(ctx, &state); err != nil {
				t.Fatal(err)
			}
		}
	}
	finishTestUsageMerge(t, ctx, f.service, &state)
	if len(state.Usage) != 0 || slices.Contains(state.Snapshot.Reasons, "usage_group_limit") {
		t.Fatal("usage remained in the checkpoint or was truncated")
	}
	rows := []storageUsage{}
	for page := 0; page < state.Snapshot.UsagePages; page++ {
		batch, _, err := inventory.Read[[]storageUsage](ctx, f.service.store, usageRunPage(state.Snapshot.SnapshotID, state.Snapshot.UsageRun, page))
		if err != nil || len(batch) > 32 {
			t.Fatalf("unbounded published page: size=%d err=%v", len(batch), err)
		}
		rows = append(rows, batch...)
	}
	if len(rows) != 66*16*4+3 || !slices.IsSortedFunc(rows, func(a, b storageUsage) int { return compareStorageUsage(a, b, true) }) {
		t.Fatalf("groups lost or incorrectly ranked: %d", len(rows))
	}
	state.Domain, state.AttributionPhase = 2, 3
	for state.NextScanAt.IsZero() {
		if err := f.service.publishStorageCollection(ctx, &state); err != nil {
			t.Fatal(err)
		}
	}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store, f.service.inventory.Store = guard, guard
	base := "/api/v1/storage/usage?category=cache_payloads&repository=team%2Fimage&dimension=image&limit=32"
	path, seen := base, map[string]bool{}
	for pages := 0; path != ""; pages++ {
		if err := ctx.Err(); err != nil {
			t.Fatal(err)
		}
		if pages > 500 {
			t.Fatal("usage cursor did not terminate")
		}
		response := diagnosticGet(t, f.service, path)
		var page storageUsagePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Truncated {
			t.Fatalf("combined query failed: %d %s", response.Code, response.Body.String())
		}
		for _, usage := range page.Items {
			if seen[usage.Name] || usage.Dimension != "image" || usage.ObjectBytes < 1 || usage.ObjectBytes > 66 || !usage.LogicalUnknown {
				t.Fatalf("incorrect or duplicated usage: %+v", usage)
			}
			seen[usage.Name] = true
		}
		path = ""
		if page.NextToken != "" {
			path = base + "&token=" + url.QueryEscape(page.NextToken)
		}
	}
	if len(seen) != 1056 || guard.writes != 0 {
		t.Fatalf("missing groups or GET writes: groups=%d writes=%d", len(seen), guard.writes)
	}
	for _, row := range rows {
		if row.Mask == 2 && row.ObjectBytes != expectedBytes {
			t.Fatalf("shared objects double-counted within repository: %+v expected=%d", row, expectedBytes)
		}
	}
}

func finishTestUsageMerge(t *testing.T, ctx context.Context, s *Service, state *storageCollection) {
	t.Helper()
	for step := 0; state.UsageMerge.Phase != "done"; step++ {
		if step > 20000 {
			t.Fatal("usage merge did not converge")
		}
		checkpoint, err := json.Marshal(state)
		if err != nil || len(checkpoint) > 16000 {
			t.Fatalf("checkpoint grew with total group cardinality: bytes=%d err=%v", len(checkpoint), err)
		}
		if err := s.advanceStorageUsageMergeUnit(ctx, state); err != nil {
			t.Fatal(err)
		}
		if step%23 == 0 {
			var replay storageCollection
			if err := json.Unmarshal(checkpoint, &replay); err != nil {
				t.Fatal(err)
			}
			if err := s.advanceStorageUsageMergeUnit(ctx, &replay); err != nil {
				t.Fatalf("replaying a committed step failed: %v", err)
			}
			actual, _ := json.Marshal(state)
			repeated, _ := json.Marshal(replay)
			if string(actual) != string(repeated) {
				t.Fatal("replayed step changed cursor or totals")
			}
		}
		raw, _ := json.Marshal(state)
		*state = storageCollection{}
		if err := json.Unmarshal(raw, state); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStorageUsageCombinedFiltersRequireOneMatchingAssociation(t *testing.T) {
	object := storageObject{Domain: "registry", Category: "registry_payloads", Associations: []storageAssociation{
		{Authority: "example.org", Repository: "first", ImageID: "image-a"},
		{Authority: "example.org", Repository: "second", ImageID: "image-b"},
	}}
	for _, group := range storageUsageGroups(object) {
		if matchesStorageUsageGroup(group, url.Values{"repository": {"first"}, "imageID": {"image-b"}}) {
			t.Fatal("query combined unrelated owners into a fabricated intersection")
		}
	}
}

func TestStorageUsageDeclaresLegacyAndAssociationTruncation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	for _, reason := range []string{"usage_group_limit", "association_limit"} {
		snapshot := newStorageCollection().Snapshot
		snapshot.UsageVersion, snapshot.AttributionCoverage, snapshot.Reasons = 0, "partial", []string{reason}
		snapshot.Usage = []storageUsage{{Domain: "state", Dimension: "category", Name: "cache_payloads", ObjectBytes: 12}}
		if err := inventory.Update(context.Background(), f.service.store, storageInventoryPrefix+"current.json", func(value *storageSnapshot) error { *value = snapshot; return nil }); err != nil {
			t.Fatal(err)
		}
		response := diagnosticGet(t, f.service, "/api/v1/storage/usage?dimension=category")
		var page storageUsagePage
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || !page.Truncated || !slices.Contains(page.Reasons, reason) || page.Coverage != "partial" {
			t.Fatalf("truncation was hidden: %d %s", response.Code, response.Body.String())
		}
	}
	state := newStorageCollection()
	addStorageUsage(&state, storageObject{Domain: "state", Category: "cache_payloads", AssociationsTruncated: true})
	if !slices.Contains(state.Snapshot.Reasons, "association_limit") || state.Snapshot.AttributionCoverage != "partial; captured inventory associations truncated" {
		t.Fatal("association truncation was not propagated to report coverage")
	}
}
