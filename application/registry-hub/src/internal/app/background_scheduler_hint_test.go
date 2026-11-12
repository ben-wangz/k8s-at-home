package app

import (
	"context"
	"testing"
	"time"
)

func TestBackgroundWakePromotesIdleAdmissionBeforeAndAfterEnqueue(t *testing.T) {
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "after"}[before], func(t *testing.T) {
			scheduler := &backgroundScheduler{capacity: 1}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			release, _ := scheduler.acquire(ctx, "", false, nil)
			defer release()
			maintenance := queueAdmission(t, scheduler, ctx, false, 1)
			hint := make(chan struct{}, 1)
			service := &Service{backgroundScheduler: scheduler, backgroundReady: map[string]chan struct{}{"execution": hint}}
			if before {
				service.signalBackgroundQueue("execution")
			}
			result := make(chan func(), 1)
			go func() {
				release, _ := scheduler.acquire(ctx, "execution", false, hint)
				result <- release
			}()
			waitForAdmissionQueue(t, scheduler, 2)
			if !before {
				service.signalBackgroundQueue("execution")
			}
			release()
			awaitAdmission(t, result)()
			awaitAdmission(t, maintenance)()
		})
	}
}

func waitForAdmissionQueue(t *testing.T, scheduler *backgroundScheduler, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		scheduler.mu.Lock()
		n := len(scheduler.waiters)
		scheduler.mu.Unlock()
		if n == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker did not queue for admission")
}
