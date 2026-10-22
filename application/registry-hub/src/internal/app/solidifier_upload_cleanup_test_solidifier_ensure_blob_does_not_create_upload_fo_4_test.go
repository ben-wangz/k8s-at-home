package app

import (
	"context"
	"errors"
	"net/http"
	"registry-hub/internal/tasks"
	"testing"
	"time"
)

func TestSolidifierEnsureBlobDoesNotCreateUploadForExistingBlobOrBusyLease(t *testing.T) {
	t.Run("existing blob", func(t *testing.T) {
		fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{})
		fixture.registry.committedData = append([]byte(nil), fixture.data...)
		err := fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
		if err != nil {
			t.Fatalf("ensureBlob() existing blob error = %v", err)
		}
		if fixture.registry.postRequests != 0 || fixture.registry.deleteRequests != 0 {
			t.Fatalf("existing blob caused POSTs=%d DELETEs=%d", fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	})

	t.Run("busy content lease", func(t *testing.T) {
		fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{})
		contentLease, err := fixture.manager.Leases().Acquire(context.Background(), "objects/"+objectLeaseKey("ghcr.io", "team/image", fixture.digest), "other-worker", time.Minute)
		if err != nil {
			t.Fatalf("acquire competing content lease: %v", err)
		}
		t.Cleanup(func() { _ = fixture.manager.Leases().Release(context.Background(), contentLease) })
		err = fixture.runBlob(context.Background(), fixture.acquireTaskLease(t))
		var failure *tasks.Failure
		if !errors.As(err, &failure) || failure.Class != "object_busy" {
			t.Fatalf("ensureBlob() error = %#v, want object_busy", err)
		}
		if fixture.registry.headRequests != 0 || fixture.registry.postRequests != 0 || fixture.registry.deleteRequests != 0 {
			t.Fatalf("busy lease caused HEAD=%d POST=%d DELETE=%d", fixture.registry.headRequests, fixture.registry.postRequests, fixture.registry.deleteRequests)
		}
	})
}

func TestSolidifierUploadCleanupCompletesBeforeWriteGateQuiesces(t *testing.T) {
	fixture := newSolidifierUploadTestFixture(t, solidifierUploadTestOptions{
		putStatuses: []int{http.StatusServiceUnavailable}, blockCleanup: true,
	})
	taskLease := fixture.acquireTaskLease(t)
	result := make(chan error, 1)
	go func() { result <- fixture.runBlob(context.Background(), taskLease) }()
	select {
	case <-fixture.registry.cleanupStarted:
	case <-time.After(2 * time.Second):
		close(fixture.registry.finishCleanup)
		t.Fatal("upload cancellation did not start")
	}
	ctx := context.Background()
	if err := fixture.solidifier.gate.BeginDrain(ctx); err != nil {
		close(fixture.registry.finishCleanup)
		t.Fatalf("begin gate drain: %v", err)
	}
	fixture.solidifier.gate.mu.Lock()
	activeWrites := fixture.solidifier.gate.active
	fixture.solidifier.gate.mu.Unlock()
	if activeWrites != 1 {
		close(fixture.registry.finishCleanup)
		t.Fatalf("active writes during blocked cleanup = %d, want 1", activeWrites)
	}
	close(fixture.registry.finishCleanup)
	if err := <-result; err == nil {
		t.Fatal("solidification unexpectedly succeeded")
	}
	if err := fixture.solidifier.gate.WaitQuiesced(ctx, time.Second); err != nil {
		t.Fatalf("gate did not quiesce after cleanup: %v", err)
	}
	if fixture.registry.active {
		t.Fatal("gate quiesced with the upload session still active")
	}
}
