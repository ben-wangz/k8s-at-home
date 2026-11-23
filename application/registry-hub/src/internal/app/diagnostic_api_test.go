package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
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

func TestHistoryCursorPinsSourceBeforeActivePhase(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	queue := workqueue.Queue{Store: f.service.store}
	for _, source := range []string{"cursor-a", "cursor-b"} {
		if _, err := queue.Enqueue(context.Background(), "notify", source, source, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = false
	first := diagnosticGet(t, f.service, "/api/v1/background/notify?limit=1")
	var workPage backgroundJobPage
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &workPage) != nil || workPage.NextToken == "" {
		t.Fatalf("first work page: status=%d body=%s", first.Code, first.Body.String())
	}
	workFilters := "/api/v1/background/notify?limit=1"
	var workCursor diagnosticCursor
	if err := f.service.decodeDiagnosticToken(workPage.NextToken, workFilters, &workCursor); err != nil {
		t.Fatal(err)
	}
	if workCursor.HistorySource != terminalWorkHistoryLegacySource || workCursor.HistoryBefore.IsZero() {
		t.Fatalf("initial work cursor = %+v, want fixed legacy source and history boundary", workCursor)
	}
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	continuedWorkPath := workFilters + "&token=" + workPage.NextToken
	for pages := 0; pages < 32; pages++ {
		continuedWork := diagnosticGet(t, f.service, continuedWorkPath)
		var continuedWorkPage backgroundJobPage
		if continuedWork.Code != http.StatusOK || json.Unmarshal(continuedWork.Body.Bytes(), &continuedWorkPage) != nil {
			t.Fatalf("continued work page: status=%d body=%s", continuedWork.Code, continuedWork.Body.String())
		}
		if strings.Contains(continuedWorkPage.Coverage, "legacy receipt fallback remains enabled") {
			break
		}
		if continuedWorkPage.NextToken == "" || pages == 31 {
			t.Fatalf("continued work page changed source or failed to complete: %s", continuedWork.Body.String())
		}
		continuedWorkPath = workFilters + "&token=" + continuedWorkPage.NextToken
	}

	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = false
	for _, id := range []string{"active-transfer-a", "active-transfer-b"} {
		writeTransferHistoryFixture(t, f.service, transferView{ID: id, Kind: "client_upload", TransferState: "uploading", ObservedAt: time.Now().UTC()})
	}
	firstTransfer := diagnosticGet(t, f.service, "/api/v1/transfers?limit=1")
	var transferPage transferHistoryPage
	if firstTransfer.Code != http.StatusOK || json.Unmarshal(firstTransfer.Body.Bytes(), &transferPage) != nil || transferPage.NextToken == "" {
		t.Fatalf("first transfer page: status=%d body=%s", firstTransfer.Code, firstTransfer.Body.String())
	}
	transferFilters := "/api/v1/transfers?limit=1"
	var transferCursor diagnosticCursor
	if err := f.service.decodeDiagnosticToken(transferPage.NextToken, transferFilters, &transferCursor); err != nil {
		t.Fatal(err)
	}
	if transferCursor.HistorySource != terminalTransferHistoryLegacySource || transferCursor.HistoryBefore.IsZero() {
		t.Fatalf("initial transfer cursor = %+v, want fixed legacy source and history boundary", transferCursor)
	}
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	continuedTransferPath := transferFilters + "&token=" + transferPage.NextToken
	for pages := 0; pages < 32; pages++ {
		continuedTransfer := diagnosticGet(t, f.service, continuedTransferPath)
		var continuedTransferPage transferHistoryPage
		if continuedTransfer.Code != http.StatusOK || json.Unmarshal(continuedTransfer.Body.Bytes(), &continuedTransferPage) != nil {
			t.Fatalf("continued transfer page: status=%d body=%s", continuedTransfer.Code, continuedTransfer.Body.String())
		}
		if strings.Contains(continuedTransferPage.Coverage, "live-source fallback remains enabled") {
			break
		}
		if continuedTransferPage.NextToken == "" || pages == 31 {
			t.Fatalf("continued transfer page changed source or failed to complete: %s", continuedTransfer.Body.String())
		}
		continuedTransferPath = transferFilters + "&token=" + continuedTransferPage.NextToken
	}
}

func TestActiveWorkHistoryUsesBoundedFortyEightHourWindow(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	boundary := time.Now().UTC().Add(-f.service.diagnosticsConfig().TerminalHistoryRetention)
	rows := []struct {
		source      string
		completedAt time.Time
	}{
		{source: "exactly-expired", completedAt: boundary},
		{source: "just-retained", completedAt: boundary.Add(time.Nanosecond)},
	}
	for _, row := range rows {
		id := workqueue.ID("notify", row.source)
		job := workqueue.Job{SchemaVersion: 1, ID: id, Kind: "notify", State: "done", CompletedAt: row.completedAt}
		if err := inventory.Update(context.Background(), f.service.store, workqueue.Key("notify", id), func(value *workqueue.Job) error {
			*value = job
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	filters := "/api/v1/background/notify?limit=10"
	cursor := diagnosticCursor{
		Query: filters, HistorySource: terminalWorkHistoryLegacySource,
		HistoryBefore: boundary, ExpiresAt: time.Now().UTC().Add(30 * time.Minute),
	}
	response := diagnosticGet(t, f.service, filters+"&token="+f.service.diagnosticToken(cursor))
	var page backgroundJobPage
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
		t.Fatalf("background page: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(page.Items) != 1 || page.Items[0].ID != workqueue.ID("notify", "just-retained") || !page.HistoryBefore.Equal(boundary) {
		t.Fatalf("background history page=%+v, want only the just-retained receipt at boundary %s", page, boundary)
	}
}

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
