package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

func TestReceiptCleanupRequiresSettledArchiveAndPersistsReplayMarker(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.cfg.Diagnostics.TerminalHistoryRetention = 30 * time.Second
	f.service.cfg.Diagnostics.FullWorkReceiptRetention = time.Minute
	kind, source := "notify", "receipt-retention"
	jobID := workqueue.ID(kind, source)
	payload, _ := json.Marshal(map[string]string{"source": source})
	receipt := workqueue.Job{
		SchemaVersion: 1, ID: jobID, Kind: kind, Payload: payload, State: "done",
		CreatedAt: time.Now().Add(-2 * time.Minute), CompletedAt: time.Now().Add(-70 * time.Second),
	}
	entry := putTestReceipt(t, f.service, receipt)
	var progress sourceSweepCheckpoint
	if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("eligible receipt remains: %v", err)
	}
	if _, _, err := inventory.Read[workqueue.CompletionMarker](ctx, f.service.store, workqueue.CompletionMarkerKey(kind, jobID)); err != nil {
		t.Fatalf("completion marker missing: %v", err)
	}
	queue := workqueue.Queue{Store: f.service.store}
	replay, err := queue.Enqueue(ctx, kind, source, map[string]string{"source": source}, time.Now().UTC())
	if err != nil || replay.State != "done" || replay.ID != jobID {
		t.Fatalf("same producer retry = %+v, error = %v", replay, err)
	}
	if _, err := queue.Enqueue(ctx, kind, source, map[string]string{"source": "conflict"}, time.Now().UTC()); !errors.Is(err, storage.ErrCorruptObject) {
		t.Fatalf("conflicting producer retry error = %v", err)
	}
}

func TestReceiptCleanupProtectsExecutionActiveAndUnsettledReceipts(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		kind       string
		active     bool
		projection bool
	}{
		{name: "execution", kind: "execution", projection: true},
		{name: "active", kind: "notify", active: true, projection: true},
		{name: "unsettled", kind: "notify", projection: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := strings.Repeat(test.name, 4)
			jobID := workqueue.ID(test.kind, source)
			payload, _ := json.Marshal(map[string]string{"source": source})
			receipt := workqueue.Job{SchemaVersion: 1, ID: jobID, Kind: test.kind, Payload: payload, State: "done", CompletedAt: time.Now().Add(-4 * 24 * time.Hour)}
			entry := putTestReceipt(t, f.service, receipt)
			if test.active {
				active := receipt
				active.State = "pending"
				if err := inventory.Update(ctx, f.service.store, workqueue.Key(test.kind, jobID), func(value *workqueue.Job) error { *value = active; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if test.projection {
				putReceiptArchiveProjection(t, f.service, entry)
			} else if err := f.service.store.Delete(ctx, diagnostics.Prefix+"archive-projections/"+diagnostics.ID(entry.Key)+".json"); err != nil {
				t.Fatal(err)
			}
			var progress sourceSweepCheckpoint
			if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.store.Head(ctx, entry.Key); err != nil {
				t.Fatalf("protected receipt missing: %v", err)
			}
		})
	}
}

func TestExecutionReceiptCleanupRequiresAndPreservesCompactFact(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	fact := inventoryExecutionFact{
		TaskID: "task-execution-compact", Kind: tasks.RootKind,
		Identity:   inventory.Identity{Authority: "ghcr.io", Repository: "team/image", Reference: "latest"},
		RootDigest: "sha256:root", Digest: "sha256:root", Generation: 3,
		ContentGeneration: 7, RootContentGeneration: 7,
	}
	source := executionSource(fact.TaskID, fact.Digest, fact.ContentGeneration)
	jobID := workqueue.ID("execution", source)
	payload, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	receipt := workqueue.Job{
		SchemaVersion: 1, ID: jobID, Kind: "execution", Payload: payload, State: "done",
		CreatedAt: time.Now().Add(-5 * 24 * time.Hour), CompletedAt: time.Now().Add(-4 * 24 * time.Hour),
	}
	entry := putTestReceipt(t, f.service, receipt)

	var progress sourceSweepCheckpoint
	if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, false); err != nil {
		t.Fatal(err)
	}
	if progress.Status.Counts["protected_execution_fact_pending"] != 1 {
		t.Fatalf("unbackfilled execution fact was not protected: %#v", progress.Status.Counts)
	}
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &progress); err != nil {
		t.Fatalf("backfill execution receipt: %v", err)
	}
	projection, _, err := inventory.Read[diagnosticReceiptArchive](ctx, f.service.store, diagnostics.Prefix+"archive-projections/"+diagnostics.ID(entry.Key)+".json")
	if err != nil || !projection.Complete || !projection.ExecutionFactProjected {
		t.Fatalf("execution archive projection = %+v, error = %v", projection, err)
	}
	if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, false); err != nil {
		t.Fatalf("clean execution receipt: %v", err)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("execution receipt remains after verified compact fact: %v", err)
	}
	compact, _, err := inventory.Read[compactExecutionFact](ctx, f.service.store, compactExecutionFactKey(jobID))
	if err != nil || compact.WorkID != jobID || compact.State != "done" || compact.Fact.TaskID != fact.TaskID {
		t.Fatalf("compact execution fact = %+v, error = %v", compact, err)
	}
	link, _, err := inventory.Read[executionSourceLink](ctx, f.service.store, executionScopePrefix(fact.Identity.ID(), fact.RootDigest, fact.RootContentGeneration)+jobID+".json")
	if err != nil {
		t.Fatalf("read execution source link: %v", err)
	}
	view, err := f.service.readExecutionFact(ctx, link)
	if err != nil || view.WorkID != jobID || view.TaskID != fact.TaskID || view.State != "done" {
		t.Fatalf("execution view after receipt cleanup = %+v, error = %v", view, err)
	}
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	response := diagnosticGet(t, f.service, "/api/v1/images/"+fact.Identity.ID()+"/execution-facts")
	var page executionFactPage
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].WorkID != jobID {
		t.Fatalf("execution-facts API after receipt cleanup = %d %s", response.Code, response.Body.String())
	}
}

func TestBlobExecutionReceiptStaysProtectedUntilSourceLinksAreVerified(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	fact := inventoryExecutionFact{
		TaskID: "intent-execution-compact", Kind: tasks.BlobKind,
		Identity:   inventory.Identity{Authority: "ghcr.io", Repository: "team/image", Reference: "latest"},
		RootDigest: "sha256:root", Digest: "sha256:blob", Generation: 3,
		ContentGeneration: 7, RootContentGeneration: 5,
	}
	source := executionSource(fact.TaskID, fact.Digest, fact.ContentGeneration)
	jobID := workqueue.ID("execution", source)
	payload, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	receipt := workqueue.Job{
		SchemaVersion: 1, ID: jobID, Kind: "execution", Payload: payload, State: "done",
		CreatedAt: time.Now().Add(-5 * 24 * time.Hour), CompletedAt: time.Now().Add(-4 * 24 * time.Hour),
	}
	entry := putTestReceipt(t, f.service, receipt)
	var progress sourceSweepCheckpoint
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &progress); err != nil {
		t.Fatalf("archive blob execution receipt: %v", err)
	}
	projection, _, err := inventory.Read[diagnosticReceiptArchive](ctx, f.service.store, diagnostics.Prefix+"archive-projections/"+diagnostics.ID(entry.Key)+".json")
	if err != nil || !projection.ExecutionFactProjected || projection.ExecutionSourcesVerified || !projection.ExecutionVerificationDone {
		t.Fatalf("blob execution source coverage = %+v, error = %v", projection, err)
	}
	if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, false); err != nil {
		t.Fatal(err)
	}
	if progress.Status.Counts["protected_execution_sources_unverified"] != 1 {
		t.Fatalf("unverified blob source was not protected: %#v", progress.Status.Counts)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); err != nil {
		t.Fatalf("unverified blob receipt was removed: %v", err)
	}
}

func TestBlobExecutionReceiptCleanupVerifiesEveryDemandGraphAssociation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	rootDigest := "sha256:" + strings.Repeat("1", 64)
	manifestDigest := "sha256:" + strings.Repeat("2", 64)
	blobDigest := "sha256:" + strings.Repeat("3", 64)
	root, _, err := f.service.tasks.EnqueueResolved(ctx, "ghcr.io", "team/image", "latest", rootDigest, "application/vnd.oci.image.manifest.v1+json", 0)
	if err != nil {
		t.Fatal(err)
	}
	rootGeneration, manifestGeneration := uint64(5), uint64(7)
	for _, node := range []tasks.GraphNode{
		{RootTaskID: root.ID, Role: tasks.GraphManifest, Digest: rootDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", ContentGeneration: &rootGeneration},
		{RootTaskID: root.ID, Role: tasks.GraphManifest, Digest: manifestDigest, MediaType: "application/vnd.oci.image.manifest.v1+json", ContentGeneration: &manifestGeneration},
	} {
		if err := f.service.records.SaveGraphNode(ctx, &node); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := protectionScopeID(root.Authority, root.Repository, manifestDigest, root.Generation)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: root.Authority, Repository: root.Repository, Digest: blobDigest}, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	intentID, demandID := intent.ID, strings.Repeat("b", 32)
	if err := f.service.records.LinkBlobDemandScope(ctx, intentID, demandID, root.ID, scope, strings.Repeat("c", 32), 128); err != nil {
		t.Fatal(err)
	}
	demands, next, err := f.service.records.ListBlobDemandRecords(ctx, intentID, "", 10)
	if err != nil || next != "" || len(demands) != 1 {
		t.Fatalf("list blob demands = %d, %q, %v", len(demands), next, err)
	}
	fact := inventoryExecutionFact{
		TaskID: intentID, Kind: tasks.BlobKind, Identity: inventoryIdentity(root.Authority, root.Repository, root.Reference),
		RootDigest: rootDigest, Digest: blobDigest, Generation: 3, ContentGeneration: 9, RootContentGeneration: rootGeneration,
	}
	payload, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := json.Marshal(inventoryExecutionProgress{Cursor: inventory.Cursor{Done: true}})
	if err != nil {
		t.Fatal(err)
	}
	job := workqueue.Job{
		SchemaVersion: 1, ID: workqueue.ID("execution", executionSource(fact.TaskID, fact.Digest, fact.ContentGeneration)),
		Kind: "execution", Payload: payload, Progress: progress, State: "done",
		CreatedAt: time.Now().Add(-5 * 24 * time.Hour), CompletedAt: time.Now().Add(-4 * 24 * time.Hour),
	}
	if err := f.service.indexBlobExecutionManifest(ctx, job, fact, root, demands[0], rootDigest, rootGeneration, true); err != nil {
		t.Fatal(err)
	}
	if err := f.service.indexBlobExecutionManifest(ctx, job, fact, root, demands[0], manifestDigest, manifestGeneration, false); err != nil {
		t.Fatal(err)
	}
	entry := putTestReceipt(t, f.service, job)
	var archive sourceSweepCheckpoint
	if err := f.service.archiveDiagnosticReceipt(ctx, entry, &archive); err != nil {
		t.Fatal(err)
	}
	projection, _, err := inventory.Read[diagnosticReceiptArchive](ctx, f.service.store, diagnostics.Prefix+"archive-projections/"+diagnostics.ID(entry.Key)+".json")
	if err != nil || !projection.ExecutionSourcesVerified {
		t.Fatalf("blob source links not verified: %+v, %v", projection, err)
	}
	if err := f.service.cleanupWorkReceipt(ctx, entry, &archive, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("verified blob execution receipt remains: %v", err)
	}
}

func TestReceiptCleanupPreviewCountsEligibleBytesWithoutWritingOrDeleting(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	kind, source := "notify", "receipt-preview"
	jobID := workqueue.ID(kind, source)
	payload, _ := json.Marshal(map[string]string{"source": source})
	receipt := workqueue.Job{SchemaVersion: 1, ID: jobID, Kind: kind, Payload: payload, State: "done", CompletedAt: time.Now().Add(-4 * 24 * time.Hour)}
	entry := putTestReceipt(t, f.service, receipt)
	putReceiptArchiveProjection(t, f.service, entry)
	var progress sourceSweepCheckpoint
	if err := f.service.cleanupWorkReceipt(ctx, entry, &progress, true); err != nil {
		t.Fatal(err)
	}
	if progress.Status.Counts["eligible"] != 1 || progress.Status.Counts["eligible_bytes"] != entry.Size {
		t.Fatalf("preview counts = %#v, want one candidate and %d bytes", progress.Status.Counts, entry.Size)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); err != nil {
		t.Fatalf("preview removed receipt: %v", err)
	}
	if _, _, err := inventory.Read[workqueue.CompletionMarker](ctx, f.service.store, workqueue.CompletionMarkerKey(kind, jobID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("preview wrote completion marker: %v", err)
	}
}

func TestReceiptCleanupPreviewCountsResetOnlyForNewRounds(t *testing.T) {
	progress := sourceSweepCheckpoint{
		Round: 3, PreviewCountRound: 3,
		Status: backgroundScanStatus{Operations: 12, Counts: map[string]int64{"eligible": 4, "eligible_bytes": 4096}},
	}
	resetReceiptCleanupPreviewCounts(&progress)
	if progress.Status.Counts["eligible"] != 4 || progress.Status.Counts["eligible_bytes"] != 4096 {
		t.Fatalf("resuming preview round lost counts: %#v", progress.Status.Counts)
	}
	progress.Round++
	resetReceiptCleanupPreviewCounts(&progress)
	if progress.PreviewCountRound != progress.Round || progress.Status.Operations != 0 || len(progress.Status.Counts) != 0 {
		t.Fatalf("new preview round retained prior counts: %+v", progress)
	}
}

func TestTerminalWorkHistoryProjectionPagesByCompletionAndExpiresDetails(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	now := time.Now().UTC()
	ids := []string{workqueue.ID("notify", "newest"), workqueue.ID("notify", "middle"), workqueue.ID("notify", "oldest")}
	completed := []time.Time{now.Add(-time.Minute), now.Add(-time.Hour), now.Add(-2 * time.Hour)}
	for index, id := range ids {
		payload, _ := json.Marshal(map[string]string{"source": fmt.Sprintf("source-%d", index)})
		job := workqueue.Job{SchemaVersion: 1, ID: id, Kind: "notify", Payload: payload, State: "done", CreatedAt: now.Add(-3 * time.Hour), CompletedAt: completed[index]}
		entry := putTestReceipt(t, f.service, job)
		projected, err := f.service.projectTerminalWorkHistory(ctx, entry.Key, sourceEntryVersion(entry), entry, job)
		if err != nil || !projected {
			t.Fatalf("project terminal job %d: projected=%t err=%v", index, projected, err)
		}
	}
	seen := make([]string, 0, len(ids))
	path := "/api/v1/background?state=done&limit=2"
	for pageNumber := 0; path != ""; pageNumber++ {
		response := diagnosticGet(t, f.service, path)
		var page backgroundJobPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("history page %d: status=%d body=%s", pageNumber, response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			seen = append(seen, item.ID)
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/background?state=done&limit=2&token=" + page.NextToken
		}
	}
	if len(seen) != len(ids) || seen[0] != ids[0] || seen[1] != ids[1] || seen[2] != ids[2] {
		t.Fatalf("terminal history order = %v, want %v", seen, ids)
	}
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+ids[1]); response.Code != http.StatusOK {
		t.Fatalf("projected detail status=%d body=%s", response.Code, response.Body.String())
	}

	expiredID := workqueue.ID("notify", "expired")
	expiredPayload, _ := json.Marshal(map[string]string{"source": "expired"})
	expired := workqueue.Job{SchemaVersion: 1, ID: expiredID, Kind: "notify", Payload: expiredPayload, State: "done", CreatedAt: now.Add(-50 * time.Hour), CompletedAt: now.Add(-49 * time.Hour)}
	putTestReceipt(t, f.service, expired)
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+expiredID); response.Code != http.StatusGone {
		t.Fatalf("expired detail status=%d body=%s", response.Code, response.Body.String())
	}
	unknownID := workqueue.ID("notify", "unknown")
	if response := diagnosticGet(t, f.service, "/api/v1/background/notify/"+unknownID); response.Code != http.StatusNotFound {
		t.Fatalf("unknown detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTerminalTransferHistoryProjectionUsesFortyEightHourWindow(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	now := time.Now().UTC()
	current := transferView{ID: diagnostics.ID("client_upload", "history-current"), Kind: "client_upload", TransferState: "complete", FinishedAt: now.Add(-time.Hour), ProgressAt: now.Add(-time.Hour), ObservedAt: now, EvidenceSource: "test"}
	writeTransferHistoryFixture(t, f.service, current)
	if err := f.service.projectTerminalTransferHistory(context.Background(), current, sourceEntryVersionFromStore(t, f.service.store, diagnostics.Prefix+"transfers/"+current.ID+".json")); err != nil {
		t.Fatal(err)
	}
	old := transferView{ID: diagnostics.ID("client_upload", "history-expired"), Kind: "client_upload", TransferState: "cancelled", FinishedAt: now.Add(-49 * time.Hour), ProgressAt: now.Add(-49 * time.Hour), ObservedAt: now, EvidenceSource: "test"}
	writeTransferHistoryFixture(t, f.service, old)
	if err := f.service.projectTerminalTransferHistory(context.Background(), old, sourceEntryVersionFromStore(t, f.service.store, diagnostics.Prefix+"transfers/"+old.ID+".json")); err != nil {
		t.Fatal(err)
	}
	seen := false
	path := "/api/v1/transfers?limit=1"
	for pages := 0; path != ""; pages++ {
		response := diagnosticGet(t, f.service, path)
		var page transferHistoryPage
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &page) != nil {
			t.Fatalf("transfer history page: status=%d body=%s", response.Code, response.Body.String())
		}
		for _, item := range page.Items {
			if item.ID == current.ID {
				seen = true
			}
			if item.ID == old.ID {
				t.Fatalf("expired transfer appeared in list: %+v", item)
			}
		}
		path = ""
		if page.NextToken != "" {
			path = "/api/v1/transfers?limit=1&token=" + page.NextToken
		}
		if pages > 3 {
			t.Fatal("transfer history pagination did not terminate")
		}
	}
	if !seen {
		t.Fatal("recent terminal transfer missing from history")
	}
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+current.ID); response.Code != http.StatusOK {
		t.Fatalf("recent transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
	oldEntry, err := f.service.store.Head(context.Background(), diagnostics.Prefix+"transfers/"+old.ID+".json")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.store.DeleteIfMatch(context.Background(), oldEntry.Key, oldEntry.Version); err != nil {
		t.Fatal(err)
	}
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+old.ID); response.Code != http.StatusGone {
		t.Fatalf("expired transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
	unknownID := diagnostics.ID("client_upload", "unknown-transfer")
	if response := diagnosticGet(t, f.service, "/api/v1/transfers/"+unknownID); response.Code != http.StatusNotFound {
		t.Fatalf("unknown transfer detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestTerminalTransferProjectionRejectsChangedSourceVersion(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	view := transferView{ID: diagnostics.ID("client_upload", "version-check"), Kind: "client_upload", TransferState: "complete", FinishedAt: time.Now().UTC(), ProgressAt: time.Now().UTC()}
	writeTransferHistoryFixture(t, f.service, view)
	if err := f.service.projectTerminalTransferHistory(context.Background(), view, "stale-source-version"); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("project stale transfer source error = %v, want condition failure", err)
	}
	if _, _, err := inventory.Read[terminalTransferHistoryRecord](context.Background(), f.service.store, terminalTransferHistoryKey(view.ID)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale transfer projection exists: %v", err)
	}
}

func TestIndexedTerminalWorkHistoryAvoidsReceiptEnumeration(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	f.service.cfg.Diagnostics.HistoryProjectionReadsEnabled = true
	now := time.Now().UTC()
	tracked := &historyListCountingStore{Store: f.service.store}
	f.service.store = tracked
	for index := 0; index < 3; index++ {
		id := workqueue.ID("notify", fmt.Sprintf("indexed-%d", index))
		payload, _ := json.Marshal(map[string]string{"source": fmt.Sprintf("indexed-source-%d", index)})
		job := workqueue.Job{SchemaVersion: 1, ID: id, Kind: "notify", Payload: payload, State: "done", CompletedAt: now.Add(-time.Duration(index+1) * time.Minute)}
		entry := putTestReceipt(t, f.service, job)
		if projected, err := f.service.projectTerminalWorkHistory(ctx, entry.Key, sourceEntryVersion(entry), entry, job); err != nil || !projected {
			t.Fatalf("project indexed work history: projected=%t err=%v", projected, err)
		}
	}
	cursor := diagnosticCursor{HistorySource: terminalWorkHistoryIndexSource, HistoryBefore: now.Add(-f.service.diagnosticsConfig().TerminalHistoryRetention)}
	rows, scanned, more, fallback, err := f.service.listTerminalWorkHistory(ctx, nilValues(), &cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || scanned != 2 || !more || fallback {
		t.Fatalf("indexed page rows=%d scanned=%d more=%t fallback=%t", len(rows), scanned, more, fallback)
	}
	budgetInitializationScans := 0
	for _, prefix := range tracked.listedPrefixes {
		if strings.HasPrefix(prefix, workqueue.Prefix+"receipts/") {
			t.Fatalf("indexed listing enumerated legacy receipt prefix %q", prefix)
		}
		if prefix == terminalWorkHistoryPrefix {
			budgetInitializationScans++
		}
	}
	if budgetInitializationScans != 1 {
		t.Fatalf("projection budget was initialized %d times, want one metadata scan", budgetInitializationScans)
	}
}

type historyListCountingStore struct {
	storage.Store
	listedPrefixes []string
}

func (s *historyListCountingStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	s.listedPrefixes = append(s.listedPrefixes, prefix)
	return s.Store.List(ctx, prefix, token, limit)
}

func nilValues() url.Values { return url.Values{} }

func putTestReceipt(t *testing.T, service *Service, job workqueue.Job) storage.Entry {
	t.Helper()
	ctx := context.Background()
	key := workqueue.ReceiptKey(job.Kind, job.ID)
	if err := inventory.Update(ctx, service.store, key, func(value *workqueue.Job) error { *value = job; return nil }); err != nil {
		t.Fatal(err)
	}
	entry, err := service.store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	putReceiptArchiveProjection(t, service, entry)
	return entry
}

func putReceiptArchiveProjection(t *testing.T, service *Service, entry storage.Entry) {
	t.Helper()
	projection := diagnosticReceiptArchive{
		SourceKey: entry.Key, SourceVersion: sourceEntryVersion(entry), ProjectionVersion: diagnosticReceiptProjectionVersion,
		HistoryProjected: true, OutboxComplete: true, OutboxDispositionVersion: 1, Complete: true,
	}
	key := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(entry.Key) + ".json"
	if err := inventory.Update(context.Background(), service.store, key, func(value *diagnosticReceiptArchive) error { *value = projection; return nil }); err != nil {
		t.Fatal(err)
	}
}

func writeTransferHistoryFixture(t *testing.T, service *Service, view transferView) {
	t.Helper()
	key := diagnostics.Prefix + "transfers/" + view.ID + ".json"
	if err := inventory.Update(context.Background(), service.store, key, func(value *transferView) error { *value = view; return nil }); err != nil {
		t.Fatal(err)
	}
}
