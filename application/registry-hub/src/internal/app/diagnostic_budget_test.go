package app

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func TestBackgroundAdmissionDoesNotConsumeExecutionBudget(t *testing.T) {
	s := &Service{backgroundSlots: make(chan struct{}, 1)}
	s.backgroundSlots <- struct{}{}
	ready := make(chan context.Context, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, release, ok := s.beginBackgroundUnit(context.Background(), "test", 200*time.Millisecond)
		if !ok {
			ready <- nil
			return
		}
		defer release()
		ready <- ctx
		<-ctx.Done()
	}()
	time.Sleep(150 * time.Millisecond)
	<-s.backgroundSlots
	ctx := <-ready
	if ctx == nil {
		t.Fatal("admission failed")
	}
	deadline, _ := ctx.Deadline()
	s.workerMu.Lock()
	started := s.workerStates["test"].LastStartedAt
	s.workerMu.Unlock()
	if deadline.Sub(started) < 190*time.Millisecond {
		t.Fatalf("execution budget consumed by admission: %s", deadline.Sub(started))
	}
	<-done
	if len(s.backgroundSlots) != 0 {
		t.Fatal("slot leaked")
	}
}

func TestBackgroundAdmissionRespectsCancellationAndExternalDeadline(t *testing.T) {
	s := &Service{backgroundSlots: make(chan struct{}, 1)}
	parent, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	ctx, release, ok := s.beginBackgroundUnit(parent, "test", time.Second)
	if !ok {
		t.Fatal("admission failed")
	}
	a, _ := parent.Deadline()
	b, _ := ctx.Deadline()
	if !a.Equal(b) {
		t.Fatal("external deadline was extended")
	}
	release()
	release()
	s.backgroundSlots <- struct{}{}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, _, ok := s.beginBackgroundUnit(cancelled, "test", time.Second); ok {
		t.Fatal("cancelled unit admitted")
	}
	if len(s.backgroundSlots) != 1 {
		t.Fatal("cancelled admission changed capacity")
	}
}

func TestEmptyStatisticsScanIsFreshWithoutEntityProgress(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	published := make(chan struct{}, 1)
	cursor := inventory.Cursor{}
	go func() {
		defer close(done)
		f.service.runMetricSweep(ctx, "empty", &cursor, nil, func(ctx context.Context) (bool, error) {
			_, _, err := cursor.Peek(ctx, f.service.store, "empty/", 32)
			return cursor.Done, err
		}, func() { published <- struct{}{} }, func() { cursor = inventory.Cursor{} })
	}()
	select {
	case <-published:
	case <-time.After(3 * time.Second):
		t.Fatal("empty scan did not finish")
	}
	deadline := time.Now().Add(time.Second)
	var value backgroundScanStatus
	for time.Now().Before(deadline) {
		value, _, _ = inventory.Read[backgroundScanStatus](context.Background(), f.service.store, "background/v1/statistics/empty.json")
		if value.Complete {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	if !value.Complete || value.LastSuccessAt.IsZero() || value.LastScanCompletedAt.IsZero() {
		t.Fatalf("empty scan is not successful: %+v", value)
	}
	if !value.ProgressAt.IsZero() || !value.LastEntityProgressAt.IsZero() {
		t.Fatalf("empty scan fabricated entity progress: %+v", value)
	}
}

type diagnosticCheckpointFailure struct{ storage.Store }

func (s diagnosticCheckpointFailure) Get(ctx context.Context, key string) (storage.Object, error) {
	if key == inventory.Prefix+"checkpoint.json" {
		return storage.Object{}, storage.ErrTimeout
	}
	return s.Store.Get(ctx, key)
}

func TestStatusSeparatesExpiredSnapshotFromCheckpointFailure(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	now := time.Now().UTC()
	snapshot := inventory.Snapshot{SchemaVersion: 1, SnapshotID: inventory.ID("expired"), ObservedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute), Building: true}
	if err := inventory.Update(context.Background(), f.service.store, inventory.Prefix+"current.json", func(value *inventory.Snapshot) error { *value = snapshot; return nil }); err != nil {
		t.Fatal(err)
	}
	f.service.store = diagnosticCheckpointFailure{f.service.store}
	status, err := f.service.inventoryStatus(context.Background())
	if err != nil || status.ReadErrors["checkpoint"] != "timeout" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if status.Background == nil || status.Background.ReadErrors["snapshot"] != "expired" {
		t.Fatalf("expired snapshot misclassified: %+v", status.Background)
	}
	if status.Background.Snapshot == nil || status.Background.Snapshot.Building || status.Background.Snapshot.Freshness.State != "expired" {
		t.Fatalf("snapshot state=%+v", status.Background.Snapshot)
	}
}
