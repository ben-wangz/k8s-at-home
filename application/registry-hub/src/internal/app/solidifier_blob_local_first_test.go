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
	if requests.Load() < 2 {
		t.Fatalf("Distribution requests = %d, want a presence check and content verification", requests.Load())
	}
	intent, err = service.records.GetBlobIntent(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if intent.State != tasks.IntentVerified {
		t.Fatalf("intent state = %q, want verified_local", intent.State)
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
