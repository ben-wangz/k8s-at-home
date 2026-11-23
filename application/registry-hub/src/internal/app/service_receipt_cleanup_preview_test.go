package app

import (
	"context"
	"encoding/json"
	"errors"

	"strings"
	"testing"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
)

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
