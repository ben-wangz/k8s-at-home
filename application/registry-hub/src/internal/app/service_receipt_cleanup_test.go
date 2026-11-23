package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/artifact"
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

func TestReceiptCleanupProtectsUnconsumedDistributionVerificationResult(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
	root := f.root("verification-receipt-retention", manifest)
	work := distributionVerificationWork{
		Authority: root.Authority, Repository: root.Repository, Digest: manifest.Digest,
		TaskID: root.ID, RoundID: root.RoundID, TaskGeneration: root.Generation,
	}
	jobID := workqueue.ID("distribution_verification", "verification-receipt-retention")
	payload, err := json.Marshal(work)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(distributionVerificationResult{FailureClass: "distribution_invalid"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := workqueue.Job{
		SchemaVersion: 1, ID: jobID, Kind: "distribution_verification", Payload: payload,
		FinalSummary: result, State: "done", CreatedAt: time.Now().Add(-5 * 24 * time.Hour),
		CompletedAt: time.Now().Add(-4 * 24 * time.Hour),
	}
	entry := putTestReceipt(t, f.service, receipt)
	if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
		task.VerificationWorkID = jobID
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var preview sourceSweepCheckpoint
	if err := f.service.cleanupWorkReceipt(ctx, entry, &preview, true); err != nil {
		t.Fatal(err)
	}
	if preview.Status.Counts["protected_verification_result_unconsumed"] != 1 {
		t.Fatalf("preview counts = %#v, want unconsumed verification protection", preview.Status.Counts)
	}
	var cleanup sourceSweepCheckpoint
	if err := f.service.cleanupWorkReceipt(ctx, entry, &cleanup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); err != nil {
		t.Fatalf("cleanup removed referenced verification result: %v", err)
	}

	if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
		task.VerificationWorkID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cleanup = sourceSweepCheckpoint{}
	if err := f.service.cleanupWorkReceipt(ctx, entry, &cleanup, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.store.Head(ctx, entry.Key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unreferenced verification result remains: %v", err)
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
