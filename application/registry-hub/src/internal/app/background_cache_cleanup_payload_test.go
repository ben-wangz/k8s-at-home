package app

import (
	"context"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestCacheCleanupSweepUsesPayloadBudgetAndCapacity(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	s := f.service
	s.backgroundSlots = make(chan struct{}, 1)
	s.backgroundSlots <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.store.Put(ctx, "cleanup-fixture/object", strings.NewReader("x"), 1, storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	leader, err := lease.NewManager(s.store).Acquire(ctx, "background/source/cache_cleanup", "payload-test", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.NewManager(s.store).Release(context.Background(), leader)
	observed, done := make(chan time.Duration, 1), make(chan struct{})
	go func() {
		defer close(done)
		s.runSourceSweepLeader(ctx, leader, "cache_cleanup", "cleanup-fixture/", func(unit context.Context, _ storage.Entry) error {
			deadline, _ := unit.Deadline()
			observed <- time.Until(deadline)
			<-unit.Done()
			return unit.Err()
		})
	}()
	defer func() { cancel(); <-done }()
	select {
	case remaining := <-observed:
		if remaining < 290*time.Second || len(s.solidificationSlots) != 1 {
			t.Fatalf("cleanup payload budget=%s active=%d", remaining, len(s.solidificationSlots))
		}
		view := workerView{workerStatus: workerStatus{ID: "cache_cleanup"}}
		s.configureWorkerBudgets(&view)
		if view.AdmissionPool != "solidification" || view.ExecutionBudgetSeconds != 300 {
			t.Fatalf("cleanup worker budget=%+v", view)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("payload cleanup waited for occupied ordinary metadata capacity")
	}
}

func TestCacheCleanupPayloadAdmissionSharesLimitAndReleases(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx, release, ok := f.service.beginSourceSweepUnit(context.Background(), "cache_cleanup")
	if !ok {
		t.Fatal("cleanup was not admitted")
	}
	defer release()
	if _, _, admitted := f.service.beginQueueUnit(context.Background(), "cache_verification"); admitted {
		t.Fatal("verification exceeded the shared payload limit")
	}
	release()
	release()
	if ctx.Err() == nil || len(f.service.solidificationSlots) != 0 {
		t.Fatal("cleanup context or payload permit leaked")
	}
}
