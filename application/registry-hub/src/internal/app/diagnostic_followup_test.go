package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"testing"
	"time"
)

func TestExecutionFactsDiscoverRootAbsentFromPublishedSnapshot(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	f.service.cfg.AdminAuth = config.Auth{Basic: []config.Credential{{Username: "observer", Password: "secret", Permissions: []string{auth.Observe}}}}
	refreshTestInventory(t, f.service)
	configuration := f.cacheObject([]byte("new configuration"), "application/vnd.oci.image.config.v1+json")
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Config: &configuration, Layers: []artifact.Descriptor{}})
	root := f.root("new-root", manifest)
	if err := f.service.inventoryStart(context.Background(), &root, manifest.Digest); err != nil {
		t.Fatal(err)
	}
	id := inventoryIdentity(root.Authority, root.Repository, root.Reference).ID()
	response := diagnosticGet(t, f.service, "/api/v1/images/"+id+"/execution-facts")
	var page executionFactPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Items[0].TaskID != root.ID || page.Items[0].FirstStartedAt.IsZero() {
		t.Fatalf("uncaptured execution source omitted: %d %s", response.Code, response.Body.String())
	}
}

func TestCachedIntentWithoutTaskExplainsActivationPending(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	object := f.cacheObject([]byte("pending activation"), "application/vnd.oci.image.layer.v1.tar")
	intent, _, err := f.service.records.EnsureBlobIntent(context.Background(), tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: object.Digest}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	view := dependencyView{CacheState: "complete", RegistryState: "unknown", ReadErrors: map[string]string{}}
	f.service.enrichIntentDependency(context.Background(), intent, &view)
	if view.Reason != "activation_pending" || view.NextAction != "wait_blob_activation" || view.TaskID != "" || view.IntentURL == "" {
		t.Fatalf("incorrect blocker: %+v", view)
	}
}

func TestOlderProjectionCompletionCannotAcknowledgeDifferentSource(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	source := inventoryManifestWork{Identity: inventoryIdentity("example.org", "repo", "latest"), Digest: "sha256:old"}
	old := workqueue.Job{ID: "old", Payload: json.RawMessage(`{"version":"old"}`), CreatedAt: time.Now().UTC().Add(time.Hour)}
	current := workqueue.Job{ID: "current", Payload: json.RawMessage(`{"version":"current"}`), CreatedAt: time.Now().UTC()}
	if err := f.service.recordProjectionRevision(ctx, &current, source, false); err != nil {
		t.Fatal(err)
	}
	if err := f.service.recordProjectionRevision(ctx, &old, source, true); err != nil {
		t.Fatal(err)
	}
	value, _, err := inventory.Read[inventoryProjectionRevision](ctx, f.service.store, projectionRevisionKey(source.Identity.ID()))
	if err != nil || value.SourceRevision != workqueue.SourceRevision(current) || value.ProjectedRevision != "" {
		t.Fatalf("opaque source ordering inferred from timestamps: %+v %v", value, err)
	}
}

func TestDistributionAttemptTracksTransportBytesWithoutClaimingCommit(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	task := tasks.Task{ID: "attempt", Authority: "example.org", Repository: "repo", BlobDigest: "sha256:blob"}
	observer := f.service.observeDistributionTransfer(task, 100)
	observer.bytes.Store(40)
	observer.finish(storage.ErrTimeout, 0, false)
	view, _, err := inventory.Read[transferAttempt](context.Background(), f.service.store, diagnostics.Prefix+"transfer-progress/"+observer.view.ID+".json")
	if err != nil || view.BodyBytesRead != 40 || view.Committed || view.ErrorClass != "timeout" || view.FinishedAt.IsZero() {
		t.Fatalf("partial attempt misreported: %+v %v", view, err)
	}
}

func TestRetrySourceRejectsRemovedIntent(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	payload, _ := json.Marshal(blobNotification{IntentID: "0123456789abcdef0123456789abcdef"})
	err := f.service.validateRetrySource(context.Background(), workqueue.Job{Kind: "notify", Payload: payload})
	if !errors.Is(err, workqueue.ErrSourceMissing) {
		t.Fatalf("missing source was not rejected: %v", err)
	}
}
