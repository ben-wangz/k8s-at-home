package app

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/workqueue"
)

func TestEventsReadsLegacyAndBatchedHistoryAcrossCursorPages(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	ctx := context.Background()
	now := time.Now().UTC()
	legacy := []diagnostics.Event{
		{ID: "legacy-one", OccurredAt: now.Add(-4 * time.Second), Component: "legacy", Operation: "test"},
		{ID: "legacy-two", OccurredAt: now.Add(-3 * time.Second), Component: "legacy", Operation: "test"},
	}
	for _, event := range legacy {
		if err := (diagnostics.Log{Store: f.service.store}).Append(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	batched := []diagnostics.Event{
		{ID: "batch-one", OccurredAt: now.Add(-2 * time.Second), Component: "batch", Operation: "test"},
		{ID: "batch-two", OccurredAt: now.Add(-time.Second), Component: "batch", Operation: "test"},
		{ID: "batch-three", OccurredAt: now, Component: "batch", Operation: "test"},
	}
	if _, err := (diagnostics.BatchLog{Store: f.service.store}).Append(ctx, batched, false); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	path := "/api/v1/events?limit=2"
	for pageNumber := 0; path != ""; pageNumber++ {
		response := diagnosticGet(t, f.service, path)
		var page eventPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("events page %d: status=%d body=%s", pageNumber, response.Code, response.Body.String())
		}
		for _, event := range page.Items {
			if seen[event.ID] {
				t.Fatalf("event repeated across pages: %s", event.ID)
			}
			seen[event.ID] = true
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/events?limit=2&token=" + page.NextToken
		}
		if pageNumber > 4 {
			t.Fatal("event pagination did not terminate")
		}
	}
	if len(seen) != len(legacy)+len(batched) {
		t.Fatalf("visible event IDs = %v", seen)
	}
}

func TestProjectionRevisionCanExplainSnapshotLag(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	config := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	root := f.root("projection", manifest)
	snapshot := refreshTestInventory(t, f.service)
	id := inventoryIdentity(root.Authority, root.Repository, root.Reference).ID()
	if err := inventory.Update(ctx, f.service.store, projectionRevisionKey(id), func(value *inventoryProjectionRevision) error {
		value.SourceRevision, value.ProjectedRevision = "new", "new"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := diagnosticGet(t, f.service, "/api/v1/images/"+id+"/projection?snapshotID="+snapshot.SnapshotID)
	var view inventoryProjectionView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || view.State != "snapshot_pending" {
		t.Fatalf("snapshot lag=%d %s", w.Code, w.Body.String())
	}
}

func TestExecutionFactRemainsVisibleBeforeLifecycleProjection(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	config := f.cacheObject([]byte("execution configuration"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	root := f.root("execution-delay", manifest)
	snapshot := refreshTestInventory(t, f.service)
	id := inventoryIdentity(root.Authority, root.Repository, root.Reference).ID()
	before := inventoryTestImage(t, f.service, snapshot, id)
	_ = f.step(root.ID)
	w := diagnosticGet(t, f.service, "/api/v1/images/"+id+"/projection?snapshotID="+snapshot.SnapshotID)
	var view inventoryProjectionView
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &view) != nil || !view.Execution.ProjectionPending || view.Execution.KnownFirstStartedAt == nil || len(view.Execution.Facts) == 0 {
		t.Fatalf("execution fact omitted: status=%d body=%s", w.Code, w.Body.String())
	}
	fact, err := (workqueue.Queue{Store: f.service.store}).Read(ctx, "execution", view.Execution.Facts[0].WorkID)
	if err != nil || !fact.CreatedAt.Equal(*view.Execution.KnownFirstStartedAt) {
		t.Fatalf("original execution time lost: fact=%+v err=%v", fact, err)
	}
	after := inventoryTestImage(t, f.service, snapshot, id)
	if after.Category != before.Category || after.Solidification.FirstStartedAt != nil {
		t.Fatal("live execution query changed captured snapshot")
	}
}
