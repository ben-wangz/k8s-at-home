package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

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
