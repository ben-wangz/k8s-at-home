package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeferredVerificationRejectsSupersededSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*tasks.Task)
		session string
	}{
		{name: "cancelled", change: func(task *tasks.Task) { task.CancelRequested = true }},
		{name: "terminal", change: func(task *tasks.Task) { task.Status = tasks.Cancelled }},
		{name: "new_attempt", change: func(task *tasks.Task) { task.AttemptID = "replacement" }},
		{name: "new_round", change: func(task *tasks.Task) { task.RoundID = "replacement" }},
		{name: "new_generation", change: func(task *tasks.Task) { task.Generation++ }},
		{name: "missing_session", session: "missing", change: func(*tasks.Task) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
			root := f.root("superseded", manifest)
			blob := contentRef{digest: artifact.CalculateSHA256([]byte("content")), size: 7}
			ctx := context.Background()
			if err := f.processor.deferBlobVerification(ctx, &root, blob, tc.session, 0, f.service.gate.State().Epoch); err == nil {
				t.Fatal("verification was not deferred")
			}
			job := readDistributionVerificationJob(t, f)
			if err := inventory.Update(ctx, f.service.store, "tasks/state/"+root.ID+".json", func(task *tasks.Task) error {
				tc.change(task)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, done, err := f.service.distributionVerificationStep(ctx, &job)
			if err != nil || !done || !job.Superseded || f.requests != 0 {
				t.Fatalf("superseded work: done=%t superseded=%t requests=%d err=%v", done, job.Superseded, f.requests, err)
			}
		})
	}
}

func TestDeferredVerificationCancellationAndTimeoutAllowRetry(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
			root := f.root("interrupted-verification", manifest)
			body := []byte("verification interrupted during payload read")
			digest := artifact.CalculateSHA256(body)
			f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
			blob := contentRef{digest: digest, size: int64(len(body))}
			ctx := context.Background()
			if err := f.processor.deferBlobVerification(ctx, &root, blob, "", 0, f.service.gate.State().Epoch); err == nil {
				t.Fatal("verification was not deferred")
			}
			job := readDistributionVerificationJob(t, f)
			entered := make(chan struct{})
			var reads atomic.Int32
			f.beforeRequest = func(request *http.Request) {
				if request.Method == http.MethodGet && reads.Add(1) == 1 {
					close(entered)
					<-request.Context().Done()
				}
			}
			var execution context.Context
			var cancel context.CancelFunc
			if timeout {
				execution, cancel = context.WithTimeout(ctx, 2*time.Second)
			} else {
				execution, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, _, err := f.service.distributionVerificationStep(execution, &job)
				finished <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("verification body read did not start")
			}
			if !timeout {
				cancel()
			}
			if err := <-finished; err == nil {
				t.Fatal("interrupted verification succeeded")
			}
			_, err := f.service.records.FindLocalObservation(ctx, root.Authority, root.Repository, tasks.GraphBlob, digest, blob.size, 0, f.service.gate.State().Epoch)
			if !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("interrupted verification published an observation: %v", err)
			}
			_, done, err := f.service.distributionVerificationStep(ctx, &job)
			if err != nil || !done || reads.Load() != 2 {
				t.Fatalf("healthy retry: done=%t reads=%d err=%v", done, reads.Load(), err)
			}
		})
	}
}

func TestDeferredVerificationReplayReusesCommittedObservation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
	root := f.root("verification-replay", manifest)
	body := []byte("verified once across a replay")
	digest := artifact.CalculateSHA256(body)
	f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
	var reads atomic.Int32
	f.beforeRequest = func(request *http.Request) {
		if request.Method == http.MethodGet {
			reads.Add(1)
		}
	}
	ctx := context.Background()
	blob := contentRef{digest: digest, size: int64(len(body))}
	if err := f.processor.deferBlobVerification(ctx, &root, blob, "", 0, f.service.gate.State().Epoch); err == nil {
		t.Fatal("verification was not deferred")
	}
	job := readDistributionVerificationJob(t, f)
	for attempt := 0; attempt < 2; attempt++ {
		_, done, err := f.service.distributionVerificationStep(ctx, &job)
		if err != nil || !done || job.Superseded {
			t.Fatalf("verification attempt %d: done=%t superseded=%t err=%v", attempt, done, job.Superseded, err)
		}
	}
	if reads.Load() != 1 {
		t.Fatalf("replayed verification performed %d body reads, want one", reads.Load())
	}
}
