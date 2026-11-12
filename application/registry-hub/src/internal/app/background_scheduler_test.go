package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func queueAdmission(t *testing.T, s *backgroundScheduler, ctx context.Context, urgent bool, pending int) <-chan func() {
	t.Helper()
	result := make(chan func(), 1)
	go func() { release, _ := s.acquire(ctx, "", urgent, nil); result <- release }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.waiters)
		s.mu.Unlock()
		if n == pending {
			return result
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("admission did not enter the pending queue")
	return nil
}

func awaitAdmission(t *testing.T, result <-chan func()) func() {
	t.Helper()
	select {
	case release := <-result:
		if release == nil {
			t.Fatal("admission was cancelled")
		}
		return release
	case <-time.After(time.Second):
		t.Fatal("admission stalled")
	}
	return nil
}

func TestBackgroundSchedulerPrioritizesWakeWithoutStarvingMaintenance(t *testing.T) {
	s := &backgroundScheduler{capacity: 1}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, _ := s.acquire(ctx, "", false, nil)
	defer release()
	maintenance := queueAdmission(t, s, ctx, false, 1)
	urgent := make([]<-chan func(), 4)
	for i := range urgent {
		urgent[i] = queueAdmission(t, s, ctx, true, i+2)
	}
	release()
	for i := 0; i < 3; i++ {
		awaitAdmission(t, urgent[i])()
	}
	awaitAdmission(t, maintenance)()
	awaitAdmission(t, urgent[3])()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != 0 || len(s.waiters) != 0 {
		t.Fatal("scheduler leaked admissions")
	}
}

func TestBackgroundSchedulerCancellationAndCapacity(t *testing.T) {
	s := &backgroundScheduler{capacity: 2}
	ctx, cancel := context.WithCancel(context.Background())
	a, _ := s.acquire(ctx, "", false, nil)
	b, _ := s.acquire(ctx, "", false, nil)
	pending := queueAdmission(t, s, ctx, true, 1)
	cancel()
	if release := <-pending; release != nil {
		t.Fatal("cancelled waiter admitted")
	}
	a()
	a()
	b()
	var active, peak atomic.Int64
	var group sync.WaitGroup
	for i := 0; i < 40; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			bounded, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			release, ok := s.acquire(bounded, "", true, nil)
			if !ok {
				t.Error("scheduler failed to recover capacity")
				return
			}
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			time.Sleep(time.Millisecond)
			active.Add(-1)
			release()
			release()
		}()
	}
	group.Wait()
	if peak.Load() > 2 || peak.Load() == 0 {
		t.Fatalf("unexpected peak concurrency %d", peak.Load())
	}
}

func TestBackgroundQueuePriorityIsIndependentOfRoundPollingDelay(t *testing.T) {
	for _, empty := range []bool{true, false} {
		delay := nextQueuePoll(0, empty)
		if delay != time.Second {
			t.Fatal("first round poll delay changed")
		}
		ctx := queueIdleContext(context.Background(), empty)
		if urgentBackgroundKind(ctx, "quota_wake") == empty {
			t.Fatalf("incorrect priority for empty=%t", empty)
		}
		if urgentBackgroundKind(ctx, "history_tasks") {
			t.Fatal("maintenance received active queue priority")
		}
	}
}
