package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strings"
	"testing"
	"time"
)

func diagnosticGet(t *testing.T, s *Service, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.SetBasicAuth("observer", "secret")
	w := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(w, r)
	return w
}

func TestDiagnosticObserverCanReadIntentsDependenciesAndWorkHistory(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	config := f.cacheObject([]byte("config"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &config, Layers: []artifact.Descriptor{}})
	root := f.root("diagnostics", manifest)
	_ = f.step(root.ID)
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: config.Digest})
	if err != nil {
		t.Fatal(err)
	}
	q := workqueue.Queue{Store: f.service.store}
	job, err := q.Enqueue(ctx, "notify", "complete-history", "payload", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	job, err = q.Claim(ctx, job)
	if err != nil {
		t.Fatal(err)
	}
	if err = q.Finish(ctx, &job, nil, true, nil); err != nil {
		t.Fatal(err)
	}
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store = guard
	f.service.inventory.Store = guard
	for _, path := range []string{"/api/v1/status", "/api/v1/workers", "/api/v1/blob-intents", "/api/v1/blob-intents/" + intent.ID, "/api/v1/tasks/" + root.ID + "/dependencies", "/api/v1/events", "/api/v1/transfers", "/api/v1/background?state=done"} {
		w := diagnosticGet(t, f.service, path)
		if w.Code != 200 {
			t.Fatalf("GET %s=%d %s", path, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "diagnosticOutbox") || strings.Contains(w.Body.String(), "internalPath") {
			t.Fatalf("private data leaked: %s", w.Body.String())
		}
	}
	page := backgroundJobPage{}
	w := diagnosticGet(t, f.service, "/api/v1/background?kind=notify&state=done")
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].ID != job.ID {
		t.Fatalf("completed history omitted: %s", w.Body.String())
	}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/background/notify/"+job.ID+"/retry", nil)
	r.SetBasicAuth("observer", "secret")
	denied := httptest.NewRecorder()
	f.service.AdminHandler().ServeHTTP(denied, r)
	if denied.Code != 403 || guard.writes != 0 {
		t.Fatalf("observer mutation: status=%d writes=%d", denied.Code, guard.writes)
	}
}

func TestDiagnosticPaginationBindsFiltersAndExpires(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	q := workqueue.Queue{Store: f.service.store}
	for _, source := range []string{"a", "b"} {
		if _, err := q.Enqueue(context.Background(), "notify", source, "payload", time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	w := diagnosticGet(t, f.service, "/api/v1/background/notify?limit=1")
	var page backgroundJobPage
	if json.Unmarshal(w.Body.Bytes(), &page) != nil || page.NextToken == "" {
		t.Fatalf("missing token: %s", w.Body.String())
	}
	w = diagnosticGet(t, f.service, "/api/v1/background/notify?limit=1&state=failed&token="+page.NextToken)
	if w.Code != 400 {
		t.Fatalf("changed filters accepted: %d", w.Code)
	}
	token := f.service.diagnosticToken(diagnosticCursor{Query: "/api/v1/background/notify?limit=1", ExpiresAt: time.Now().Add(-time.Second)})
	w = diagnosticGet(t, f.service, "/api/v1/background/notify?limit=1&token="+token)
	if w.Code != 410 {
		t.Fatalf("expired token=%d", w.Code)
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
