package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedVerificationKeepsIndependentSessionJournals(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	manifest := f.cacheManifest(artifact.Manifest{SchemaVersion: 2, MediaType: ociManifestMediaType})
	roots := []tasks.Task{f.root("session-first", manifest), f.root("session-second", manifest)}
	body := []byte("shared content with independent protection journals")
	digest := artifact.CalculateSHA256(body)
	f.objects[digest] = stagedAcceptanceObject{body: body, mediaType: "application/octet-stream"}
	blob := contentRef{digest: digest, size: int64(len(body))}
	path := "/v2/" + f.processor.internalName(roots[0].Authority, roots[0].Repository) + "/blobs/" + digest
	remote := protectionRemoteWrite{Kind: "blob", Path: path, Digest: digest, Size: blob.size}
	sessions := []string{mustGateTestID(t), mustGateTestID(t)}
	jobs := make([]workqueue.Job, len(roots))
	for i := range roots {
		if err := f.service.gate.AdmitProtection(ctx, sessions[i], mustGateTestID(t), roots[i].ID, blob.size, 10, 1<<20); err != nil {
			t.Fatal(err)
		}
		if err := f.service.gate.MarkProtectionWriteStarted(ctx, sessions[i], remote); err != nil {
			t.Fatal(err)
		}
		if err := inventory.Update(ctx, f.service.store, "tasks/state/"+roots[i].ID+".json", func(task *tasks.Task) error {
			task.ProtectionSessionID = sessions[i]
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		roots[i].ProtectionSessionID = sessions[i]
		if err := f.processor.deferBlobVerification(ctx, &roots[i], blob, sessions[i], 0, f.service.gate.State().Epoch); err == nil {
			t.Fatal("verification was not deferred")
		}
		job, err := f.service.backgroundQueue().Read(ctx, "distribution_verification", roots[i].VerificationWorkID)
		if err != nil {
			t.Fatal(err)
		}
		jobs[i] = job
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(proceed) })
	var reads atomic.Int32
	f.beforeRequest = func(request *http.Request) {
		if request.Method == http.MethodGet && reads.Add(1) == 1 {
			close(entered)
			<-proceed
		}
	}
	finished := make(chan error, 1)
	go func() {
		_, _, err := f.service.distributionVerificationStep(ctx, &jobs[0])
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first verification did not start")
	}
	_, done, err := f.service.distributionVerificationStep(ctx, &jobs[1])
	var waiting *workqueue.DependencyWait
	if done || !errors.As(err, &waiting) {
		t.Fatalf("second verifier did not wait for the object lease: done=%t err=%v", done, err)
	}
	if err := inventory.Update(ctx, f.service.store, "tasks/state/"+roots[1].ID+".json", func(task *tasks.Task) error {
		task.CancelRequested = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(proceed) })
	if err := <-finished; err != nil {
		t.Fatalf("first verifier failed after the other task was cancelled: %v", err)
	}
	_, done, err = f.service.distributionVerificationStep(ctx, &jobs[1])
	if err != nil || !done || !jobs[1].Superseded || reads.Load() != 1 {
		t.Fatalf("cancelled verifier: done=%t superseded=%t reads=%d err=%v", done, jobs[1].Superseded, reads.Load(), err)
	}
	for _, sessionID := range sessions {
		session, err := f.service.gate.readProtectionSession(ctx, sessionID)
		if err != nil || !session.UncertainRemoteResult || session.RemoteWrite == nil || *session.RemoteWrite != remote {
			t.Fatalf("verification changed session %s journal: %+v, %v", sessionID, session, err)
		}
	}
	if err := f.service.gate.MarkProtectionWriteResolved(ctx, sessions[0], remote); err != nil {
		t.Fatal(err)
	}
	remaining, err := f.service.gate.readProtectionSession(ctx, sessions[1])
	if err != nil || !remaining.UncertainRemoteResult || remaining.RemoteWrite == nil {
		t.Fatalf("resolving one session changed the other: %+v, %v", remaining, err)
	}
	if err := f.service.gate.BeginDrain(ctx); !errors.Is(err, ErrGCProtected) {
		t.Fatalf("GC proceeded with a protected session: %v", err)
	}
}
