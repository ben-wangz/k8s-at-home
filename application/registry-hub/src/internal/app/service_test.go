package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPingWithRetryWaitsForDependencyAndHonorsDeadline(t *testing.T) {
	t.Run("eventual readiness", func(t *testing.T) {
		attempts := 0
		err := pingWithRetry(context.Background(), func(context.Context) error {
			attempts++
			if attempts < 3 {
				return errors.New("connection refused")
			}
			return nil
		}, time.Millisecond)
		if err != nil {
			t.Fatalf("pingWithRetry() error = %v", err)
		}
		if attempts != 3 {
			t.Fatalf("ping attempts = %d, want 3", attempts)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer cancel()
		attempts := 0
		pingErr := errors.New("connection refused")
		err := pingWithRetry(ctx, func(context.Context) error {
			attempts++
			return pingErr
		}, time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), pingErr.Error()) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("pingWithRetry() error = %v, want last probe error and deadline", err)
		}
		if attempts < 2 {
			t.Fatalf("ping attempts = %d, want retries before deadline", attempts)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		attempts := 0
		err := pingWithRetry(ctx, func(context.Context) error {
			attempts++
			return errors.New("connection refused")
		}, time.Millisecond)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pingWithRetry() error = %v, want cancellation", err)
		}
		if attempts != 0 {
			t.Fatalf("ping attempts = %d, want 0", attempts)
		}
	})
}

func TestRepairWorkReservesOneSolidificationSlot(t *testing.T) {
	service := &Service{solidificationSlots: make(chan struct{}, 4)}
	service.setRepairPending(true)
	repairReleases := make([]func(), 0, 3)
	for range 3 {
		release, ok := service.acquireSolidificationSlot(context.Background(), true)
		if !ok {
			t.Fatal("repair could not use one of three non-reserved slots")
		}
		repairReleases = append(repairReleases, release)
	}
	if _, ok := service.acquireSolidificationSlot(context.Background(), true); ok {
		t.Fatal("repair consumed the slot reserved for new solidification")
	}
	releaseNew, ok := service.acquireSolidificationSlot(context.Background(), false)
	if !ok {
		t.Fatal("new solidification could not use its reserved slot")
	}
	if len(service.solidificationSlots) != 4 {
		t.Fatalf("active slots = %d, want global limit 4", len(service.solidificationSlots))
	}
	if _, ok := service.acquireSolidificationSlot(context.Background(), false); ok {
		t.Fatal("new solidification exceeded its reserved slot while repair was pending")
	}
	releaseNew()
	for _, release := range repairReleases {
		release()
	}

	service.setRepairPending(false)
	var releases []func()
	for range 4 {
		release, ok := service.acquireSolidificationSlot(context.Background(), false)
		if !ok {
			t.Fatal("new solidification did not use all slots without pending repair")
		}
		releases = append(releases, release)
	}
	if _, ok := service.acquireSolidificationSlot(context.Background(), false); ok {
		t.Fatal("active solidification exceeded the global worker limit")
	}
	for _, release := range releases {
		release()
	}
}

func TestBlockedRepairWorkersCannotStarveNewSolidification(t *testing.T) {
	service := &Service{solidificationSlots: make(chan struct{}, 4)}
	service.setRepairPending(true)
	workerRelease := make(chan struct{})
	workerDone := make(chan struct{}, 4)
	acquired := make(chan bool, 4)

	for range 3 {
		go func() {
			release, ok := service.acquireSolidificationSlot(context.Background(), true)
			acquired <- ok
			if ok {
				<-workerRelease
				release()
			}
			workerDone <- struct{}{}
		}()
	}
	for range 3 {
		if !<-acquired {
			t.Fatal("repair worker could not acquire a non-reserved slot")
		}
	}

	if _, ok := service.acquireSolidificationSlot(context.Background(), true); ok {
		t.Fatal("a fourth blocked repair worker acquired a slot")
	}
	go func() {
		release, ok := service.acquireSolidificationSlot(context.Background(), false)
		acquired <- ok
		if ok {
			<-workerRelease
			release()
		}
		workerDone <- struct{}{}
	}()
	select {
	case ok := <-acquired:
		if !ok {
			t.Fatal("new solidification could not acquire the slot reserved during repair")
		}
	case <-time.After(time.Second):
		t.Fatal("new solidification was starved by blocked repair workers")
	}
	if active := len(service.solidificationSlots); active != 4 {
		t.Fatalf("active solidification workers = %d, want 4", active)
	}

	close(workerRelease)
	for range 4 {
		<-workerDone
	}
}
