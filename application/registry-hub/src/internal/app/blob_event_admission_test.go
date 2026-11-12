package app

import (
	"context"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestCachedBlobEventSurvivesAdmissionTimeout(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	blob := f.cacheObject([]byte("cache-ready admission replay"), "application/octet-stream")
	identity := tasks.BlobIdentity{Authority: "ghcr.io", Repository: "team/image", Digest: blob.Digest}
	scheduler := &backgroundScheduler{capacity: 1}
	f.service.backgroundScheduler = scheduler
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	release, _ := scheduler.acquire(ctx, "held", false, nil)
	defer release()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.service.processCachedBlobEvent(ctx, identity)
	}()
	defer func() { cancel(); <-done }()
	yielded := false
	for ctx.Err() == nil {
		f.service.workerMu.Lock()
		state := f.service.workerStates["activation_events"]
		f.service.workerMu.Unlock()
		if state.AdmissionYields > 0 {
			yielded = true
			break
		}
		select {
		case <-done:
			t.Fatal("cache-ready event was discarded after its admission expired")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if !yielded {
		t.Fatal("did not observe the admission deadline")
	}
	release()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("cache-ready event was not resumed after capacity became available")
	}
	intent, err := f.service.records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.tasks.Get(ctx, intent.ID); err != nil {
		t.Fatalf("retained event did not create the blob task: %v", err)
	}
}

func TestCachedBlobEventsHaveRunnablePriority(t *testing.T) {
	if !urgentBackgroundKind(context.Background(), "activation_events") {
		t.Fatal("a cache-ready event must not enter maintenance admission")
	}
}
