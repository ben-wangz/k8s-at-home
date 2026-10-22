package app

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func (fixture *closurePublicationFixture) serveManifest(writer http.ResponseWriter, request *http.Request, reference string) {
	if request.Method == http.MethodGet {
		fixture.objectMu.Lock()
		object, exists := fixture.objects[reference]
		fixture.objectMu.Unlock()
		if !exists {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`)
			return
		}
		fixture.verifiedReads.Add(1)
		writer.Header().Set("Content-Type", object.mediaType)
		writer.Header().Set("Docker-Content-Digest", object.digest)
		_, _ = writer.Write(object.body)
		return
	}
	if request.Method != http.MethodPut {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "invalid body", http.StatusBadRequest)
		return
	}
	digest := artifact.CalculateSHA256(body)
	fixture.objectMu.Lock()
	fixture.objects[reference] = closureRegistryObject{body: body, mediaType: request.Header.Get("Content-Type"), digest: digest}
	fixture.objectMu.Unlock()
	writer.Header().Set("Docker-Content-Digest", digest)
	writer.Header().Set("Location", request.URL.Path)
	writer.WriteHeader(http.StatusCreated)
}

func (fixture *closurePublicationFixture) startRootTask(t *testing.T) {
	t.Helper()
	task, _, err := fixture.manager.Enqueue(fixture.ctx, fixture.authority, fixture.repository, "latest")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.manager.SetDiscovered(fixture.ctx, &task, fixture.rootDigest, ociIndexMediaType); err != nil {
		t.Fatal(err)
	}
	fixture.task, err = fixture.manager.Get(fixture.ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.taskLease, err = fixture.manager.Leases().Acquire(fixture.ctx, "tasks/"+task.ID, "test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.manager.Leases().Release(fixture.ctx, fixture.taskLease) })
	fixture.task.Status = tasks.Running
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.task, fixture.taskLease); err != nil {
		t.Fatal(err)
	}
}

func (fixture *closurePublicationFixture) waitForBlobDependency(t *testing.T) {
	t.Helper()
	rootErr := fixture.processor.Process(fixture.ctx, &fixture.task, fixture.taskLease)
	var rootYield *tasks.Yield
	if !errors.As(rootErr, &rootYield) || rootYield.Status != tasks.WaitingDeps {
		t.Fatalf("first root processing result = %v, want waiting_dependencies", rootErr)
	}
	if _, exists := fixture.objects["latest"]; exists {
		t.Fatal("root reference was published before its blob dependency was available in Distribution")
	}
	fixture.task.Status = rootYield.Status
	fixture.task.WaitReason = rootYield.Reason
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.task, fixture.taskLease); err != nil {
		t.Fatal(err)
	}
	intent, err := fixture.service.records.GetBlobIntentByIdentity(fixture.ctx, tasks.BlobIdentity{Authority: fixture.authority, Repository: fixture.repository, Digest: fixture.blobDigest})
	if err != nil {
		t.Fatal(err)
	}
	fixture.blobTask, err = fixture.manager.Get(fixture.ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.blobLease, err = fixture.manager.Leases().Acquire(fixture.ctx, "tasks/"+fixture.blobTask.ID, "blob-test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.manager.Leases().Release(fixture.ctx, fixture.blobLease) })
	fixture.blobTask.Status = tasks.Running
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.blobTask, fixture.blobLease); err != nil {
		t.Fatal(err)
	}
}

func (fixture *closurePublicationFixture) processBlobAndCleanup(t *testing.T) {
	t.Helper()
	blobErr := fixture.processor.Process(fixture.ctx, &fixture.blobTask, fixture.blobLease)
	var blobYield *tasks.Yield
	if !errors.As(blobErr, &blobYield) || blobYield.Status != tasks.VerifiedLocal {
		t.Fatalf("blob processing result = %v, want verified_local", blobErr)
	}
	blobTask, err := fixture.manager.Get(fixture.ctx, fixture.blobTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.blobTask = blobTask
	fixture.blobTask.Status = blobYield.Status
	fixture.blobTask.WaitReason = blobYield.Reason
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.blobTask, fixture.blobLease); err != nil {
		t.Fatal(err)
	}
	fixture.processor.TaskYielded(fixture.ctx, &fixture.blobTask)
	removed, err := fixture.cache.Cleanup(fixture.ctx, time.Now().Add(time.Minute), 100)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup after verified blob removed=%d err=%v, want 1", removed, err)
	}
	retained, err := fixture.cache.GetAnyRetained(fixture.ctx, fixture.authority, fixture.repository, fixture.blobDigest)
	if err == nil {
		_ = retained.Body.Close()
	}
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("verified blob cache lookup error = %v, want not found after cleanup", err)
	}
	if err := storeDataObjectCount(t, fixture.ctx, fixture.store, 2); err != nil {
		t.Fatal(err)
	}
}

func (fixture *closurePublicationFixture) resumeRootAndVerify(t *testing.T) {
	t.Helper()
	var err error
	fixture.task, err = fixture.manager.Get(fixture.ctx, fixture.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.task.Status = tasks.Running
	fixture.task.WaitReason = ""
	if err := fixture.manager.SaveProgress(fixture.ctx, &fixture.task, fixture.taskLease); err != nil {
		t.Fatal(err)
	}
	if err := fixture.processor.Process(fixture.ctx, &fixture.task, fixture.taskLease); err != nil {
		t.Fatalf("resume root after blob verification: %v (%#v)", err, err)
	}
	if fixture.task.Checkpoints[fixture.rootDigest] != "verified" || fixture.task.Checkpoints[fixture.childDigest] != "verified" {
		t.Fatalf("solidification checkpoints = %#v", fixture.task.Checkpoints)
	}
	if fixture.verifiedReads.Load() < 4 {
		t.Fatalf("Distribution verification reads = %d, want blob, child, root digest, and root tag", fixture.verifiedReads.Load())
	}
	fixture.assertRootPublished(t)
	removed, err := fixture.cache.Cleanup(fixture.ctx, time.Now().Add(time.Minute), 100)
	if err != nil || removed != 3 {
		t.Fatalf("cleanup after root anchor removed=%d err=%v, want 3", removed, err)
	}
	if err := storeDataObjectCount(t, fixture.ctx, fixture.store, 0); err != nil {
		t.Fatal(err)
	}
}

func (fixture *closurePublicationFixture) assertRootPublished(t *testing.T) {
	t.Helper()
	fixture.objectMu.Lock()
	rootByTag, exists := fixture.objects["latest"]
	fixture.objectMu.Unlock()
	if !exists || rootByTag.digest != fixture.rootDigest || !bytes.Equal(rootByTag.body, fixture.rootBody) {
		t.Fatalf("root tag was not published with the verified root: %#v", rootByTag)
	}
}
