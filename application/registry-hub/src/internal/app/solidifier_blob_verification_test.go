package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentBlobVerificationSharesReadback(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "root_first", true: "blob_first"}[borrowed], func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			body := []byte("one independently verified Distribution layer")
			digest := artifact.CalculateSHA256(body)
			f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
			first, second := f.root("first", manifest), f.root("second", manifest)
			blob := contentRef{digest: digest, size: int64(len(body)), mediaType: "application/octet-stream"}
			path := "/v2/" + f.processor.internalName(first.Authority, first.Repository) + "/blobs/" + digest
			entered, proceed := make(chan struct{}), make(chan struct{})
			var reads atomic.Int32
			f.beforeRequest = func(r *http.Request) {
				if r.Method == http.MethodGet {
					if reads.Add(1) == 1 {
						close(entered)
						<-proceed
					}
				}
			}
			ctx := context.Background()
			var firstJob *workqueue.Job
			if borrowed {
				leaseCtx, release, err := acquireContentLease(ctx, f.service.tasks.Leases(), first.Authority, first.Repository, digest, "upload")
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				ctx = leaseCtx
			} else {
				err := f.processor.verifyOrReuseLocalBlob(ctx, &first, blob, "", path)
				var wait *tasks.Yield
				if !errors.As(err, &wait) {
					t.Fatalf("root did not defer independent verification: %v", err)
				}
				job := readDistributionVerificationJob(t, f)
				firstJob = &job
			}
			done := make(chan error, 1)
			go func() {
				if firstJob == nil {
					done <- f.processor.verifyOrReuseLocalBlob(ctx, &first, blob, "", path)
					return
				}
				_, _, err := f.service.distributionVerificationStep(ctx, firstJob)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first verifier did not start")
			}
			waiterCtx, cancelWaiter := context.WithCancel(context.Background())
			err := f.processor.verifyOrReuseLocalBlob(waiterCtx, &second, blob, "", path)
			cancelWaiter()
			var wait *tasks.Yield
			if !errors.As(err, &wait) || wait.Reason != "distribution_verification" {
				close(proceed)
				t.Fatalf("concurrent caller must yield without another read: %v", err)
			}
			close(proceed)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := f.processor.verifyOrReuseLocalBlob(context.Background(), &second, blob, "", path); err != nil {
				t.Fatal(err)
			}
			if reads.Load() != 1 {
				t.Fatalf("readbacks = %d, want one", reads.Load())
			}
			entries, _, err := f.service.store.List(context.Background(), workqueue.Prefix+"jobs/distribution_verification/", "", 10)
			wantJobs := 1
			if !borrowed {
				wantJobs = 2
			}
			if err != nil || len(entries) != wantJobs {
				t.Fatalf("durable verification handoff = %d, %v", len(entries), err)
			}
		})
	}
}

func TestBlobVerificationRejectsLostFenceAndCorruptBody(t *testing.T) {
	for _, mode := range []string{"lost_fence", "corrupt_body", "truncated_body"} {
		t.Run(mode, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
			root := f.root("invalid", manifest)
			body := []byte("expected payload")
			digest := artifact.CalculateSHA256(body)
			actual := body
			if mode == "corrupt_body" {
				actual = []byte("corrupt payload!")
			} else if mode == "truncated_body" {
				actual = body[:len(body)-1]
			}
			f.objects[digest] = stagedAcceptanceObject{body: actual, mediaType: "application/octet-stream"}
			blob := contentRef{digest: digest, size: int64(len(body))}
			path := "/v2/" + f.processor.internalName(root.Authority, root.Repository) + "/blobs/" + digest
			var lost atomic.Bool
			f.beforeRequest = func(r *http.Request) {
				if r.Method == http.MethodGet && mode == "lost_fence" {
					lost.Store(true)
				}
			}
			ctx := inventory.WithWriteGuard(context.Background(), func(context.Context) error {
				if lost.Load() {
					return storage.ErrConditionFailed
				}
				return nil
			})
			leaseCtx, release, err := acquireContentLease(ctx, f.service.tasks.Leases(), root.Authority, root.Repository, digest, "invalid-test")
			if err != nil {
				t.Fatal(err)
			}
			if err := f.processor.verifyOrReuseLocalBlob(leaseCtx, &root, blob, "", path); err == nil {
				t.Fatal("verification succeeded after invalid read or fence")
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
			_, err = f.service.records.FindLocalObservation(context.Background(), root.Authority, root.Repository, tasks.GraphBlob, digest, blob.size, 0, f.service.gate.State().Epoch)
			if !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("invalid attempt published observation: %v", err)
			}
			f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
			leaseCtx, release, err = acquireContentLease(context.Background(), f.service.tasks.Leases(), root.Authority, root.Repository, digest, "retry-test")
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err := f.processor.verifyOrReuseLocalBlob(leaseCtx, &root, blob, "", path); err != nil {
				t.Fatalf("healthy retry failed: %v", err)
			}
		})
	}
}

func readDistributionVerificationJob(t *testing.T, f *stagedAcceptanceFixture) workqueue.Job {
	t.Helper()
	ctx := context.Background()
	entries, _, err := f.service.store.List(ctx, workqueue.Prefix+"jobs/distribution_verification/", "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("handoff: %v, %v", entries, err)
	}
	id := entries[0].Key[len(workqueue.Prefix+"jobs/distribution_verification/"):]
	id = id[:len(id)-len(".json")]
	job, err := f.service.backgroundQueue().Read(ctx, "distribution_verification", id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestOrdinaryAdmissionDefersDistributionReadback(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType, Layers: []artifact.Descriptor{}})
	root := f.root("defer", manifest)
	body := []byte("payload outside the metadata admission")
	digest := artifact.CalculateSHA256(body)
	f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
	blob := contentRef{digest: digest, size: int64(len(body))}
	ctx := context.WithValue(context.Background(), backgroundParentKey{}, context.Background())
	err := f.processor.verifyOrReuseLocalBlob(ctx, &root, blob, "", "/unused")
	var wait *tasks.Yield
	if !errors.As(err, &wait) || wait.Reason != "distribution_verification" || f.requests != 0 {
		t.Fatalf("metadata caller performed payload work: %v, requests=%d", err, f.requests)
	}
	entries, _, err := f.service.store.List(ctx, workqueue.Prefix+"jobs/distribution_verification/", "", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("handoff: %v, %v", entries, err)
	}
	id := entries[0].Key[len(workqueue.Prefix+"jobs/distribution_verification/"):]
	id = id[:len(id)-len(".json")]
	job, err := f.service.backgroundQueue().Read(context.Background(), "distribution_verification", id)
	if err != nil {
		t.Fatal(err)
	}
	_, complete, err := f.service.distributionVerificationStep(context.Background(), &job)
	if err != nil || !complete {
		t.Fatalf("payload executor = complete %t, %v", complete, err)
	}
	if _, err := f.service.records.FindLocalObservation(context.Background(), root.Authority, root.Repository, tasks.GraphBlob, digest, int64(len(body)), 0, f.service.gate.State().Epoch); err != nil {
		t.Fatalf("missing independent observation: %v", err)
	}
}
