package app

import (
	"context"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestScanWakeNeverPostponesWorkOrBypassesRetry(t *testing.T) {
	now := time.Now()
	deadline := now.Add(time.Second)
	for n := range 100 {
		deadline = advanceScanWake(deadline, time.Time{}, now.Add(time.Duration(n)*time.Millisecond))
	}
	if !deadline.Equal(now.Add(time.Second)) {
		t.Fatal("continuous events postponed existing work")
	}
	retry := now.Add(30 * time.Second)
	if got := advanceScanWake(now.Add(time.Minute), retry, now); !got.Equal(retry) {
		t.Fatal("wake bypassed the retry lower bound")
	}
	if got := advanceScanWake(time.Time{}, retry, now); !got.IsZero() {
		t.Fatal("wake postponed immediately runnable work")
	}
}

func TestContinuousWakeAllowsSourceAndStatisticsProgress(t *testing.T) {
	for _, kind := range []string{"source_tasks", "statistics_tasks"} {
		t.Run(kind, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			s, ctx := f.service, context.Background()
			s.backgroundWake = newBackgroundScanWakeChannels()
			holder, err := lease.NewManager(s.store).Acquire(ctx, "wake-progress", "test", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.NewManager(s.store).Release(ctx, holder)
			deadline := time.Now().Add(100 * time.Millisecond)
			if kind == "source_tasks" {
				err = inventory.Update(ctx, s.store, sourceSweepKey(kind), func(value *sourceSweepCheckpoint) error { value.NextRound = deadline; return nil })
			} else {
				err = inventory.Update(ctx, s.store, metricCheckpointKey("tasks"), func(value *metricSweepCheckpoint) error {
					value.NextRound = deadline
					value.Accumulator = []byte("0")
					return nil
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := inventory.Update(ctx, s.store, "wake-fixture/one.json", func(value *string) error { *value = "one"; return nil }); err != nil {
				t.Fatal(err)
			}
			bounded, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			progress := make(chan struct{}, 1)
			notify := func() {
				select {
				case progress <- struct{}{}:
				default:
				}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				if kind == "source_tasks" {
					s.runSourceSweepLeader(bounded, holder, kind, "wake-fixture/", func(context.Context, storage.Entry) error { notify(); return nil })
				} else {
					accumulator := accumulatorFor(new(int))
					cursor := inventory.Cursor{}
					s.runMetricSweepLeader(bounded, holder, "tasks", &cursor, accumulator, func(context.Context) (bool, error) { notify(); return false, nil }, func() {}, func() {})
				}
			}()
			ticker := time.NewTicker(20 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-progress:
					cancel()
					<-done
					return
				case <-ticker.C:
					s.signalBackgroundScan(kind)
				case <-bounded.Done():
					<-done
					t.Fatal("continuous notifications starved scheduled scan")
				}
			}
		})
	}
}
