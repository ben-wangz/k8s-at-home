package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlobWorkerUsesDistributionWhenCacheWasCleaned(t *testing.T) {
	ctx := context.Background()
	service, manager, _ := newDiscoveryTestService(t, &scriptedUpstream{})
	gate, err := newWriteGate(ctx, service.store)
	if err != nil {
		t.Fatal(err)
	}
	service.gate = gate
	body := []byte("already committed to Distribution")
	digest := artifact.CalculateSHA256(body)
	var requests atomic.Int32
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.Header().Set("Docker-Content-Digest", digest)
		writer.WriteHeader(http.StatusOK)
		if request.Method == http.MethodGet {
			_, _ = writer.Write(body)
		}
	}))
	intent, _, err := service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "previous-cache-version", int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "cache-cleanup-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	processor := &solidifier{service: service, tasks: manager, gate: gate, cache: service.cache, registry: service.registry}
	err = processor.processBlob(ctx, &task, taskLease)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.VerifiedLocal {
		t.Fatalf("worker result = %v, want verified_local while awaiting manifest anchor", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("Distribution requests = %d, want one digest-indexed HEAD", requests.Load())
	}
	intent, err = service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != tasks.IntentVerified {
		t.Fatalf("intent state = %q, want verified_local", intent.State)
	}
}

func TestBlobWorkerReplaysAnchoredRootAfterVerification(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	_, blob, _ := publishAnchoredTestPlatform(f, "recovery-after-blob-verified")
	root := completedAnchoredRootForBlob(t, f, blob.Digest)
	ctx := context.Background()
	generation, err := f.service.generation(ctx, root.Authority, root.Repository, blob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	intent, _, err := f.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{
		Authority: root.Authority, Repository: root.Repository, Digest: blob.Digest,
	}, generation.Generation, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	cached, err := f.service.cache.GetAnyRetained(ctx, root.Authority, root.Repository, blob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	_ = cached.Body.Close()
	intent, err = f.service.records.SetBlobIntentReady(ctx, intent.ID, blob.MediaType, cached.DataVersion, blob.Size)
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := f.service.tasks.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := f.service.tasks.Leases().Acquire(ctx, "tasks/"+task.ID, "post-verification-root-replay-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer f.service.tasks.Leases().Release(ctx, taskLease)

	err = f.processor.processBlob(ctx, &task, taskLease)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.VerifiedLocal {
		t.Fatalf("blob worker result = %v (yield=%+v), want verified_local", err, yielded)
	}
	verified, err := f.service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != tasks.IntentVerified {
		t.Fatalf("blob intent state = %q, want verified", verified.State)
	}
	active := activeRootForReference(t, f, root.Reference)
	if len(active) != 1 || active[0].ID == root.ID || active[0].RootDigest != root.RootDigest || active[0].Generation != root.Generation {
		t.Fatalf("active roots after blob verification = %#v; want one replacement for the current root", active)
	}
}

func TestBlobWorkerYieldsAfterDistributionAndCacheMiss(t *testing.T) {
	ctx := context.Background()
	service, manager, _ := newDiscoveryTestService(t, &scriptedUpstream{})
	gate, err := newWriteGate(ctx, service.store)
	if err != nil {
		t.Fatal(err)
	}
	service.gate = gate
	body := []byte("not present locally")
	digest := artifact.CalculateSHA256(body)
	var requests atomic.Int32
	service.registry = newLocalRegistry(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprint(writer, `{"errors":[{"code":"BLOB_UNKNOWN"}]}`)
	}))
	intent, _, err := service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: digest}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := service.records.LinkBlobDemand(ctx, intent.ID, intent.ID); err != nil {
		t.Fatal(err)
	}
	intent, err = service.records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "missing-cache-version", int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.EnqueueBlob(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	taskLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "double-miss-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Leases().Release(ctx, taskLease)
	processor := &solidifier{service: service, tasks: manager, gate: gate, cache: service.cache, registry: service.registry}
	err = processor.processBlob(ctx, &task, taskLease)
	var yielded *tasks.Yield
	if !errors.As(err, &yielded) || yielded.Status != tasks.WaitingCache {
		t.Fatalf("worker result = %v, want waiting_cache", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("Distribution requests = %d, want one HEAD and no background payload GET", requests.Load())
	}
	intent, err = service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != tasks.IntentWaitingCache {
		t.Fatalf("intent state = %q, want waiting_cache", intent.State)
	}
	if len(gate.State().SessionIDs) != 0 {
		t.Fatalf("empty direct blob protection remains active: %v", gate.State().SessionIDs)
	}
}
