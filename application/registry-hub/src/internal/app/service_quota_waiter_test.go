package app

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
)

func TestWakeDistributionQuotaWaiterCreditsReplaceableReservation(t *testing.T) {
	for _, test := range []struct {
		name             string
		requestedBytes   int64
		replaceableBytes int64
		replaceableSlots int
		wantStatus       tasks.State
	}{
		{name: "replacement fits", requestedBytes: 950, replaceableBytes: 100, replaceableSlots: 1, wantStatus: tasks.Pending},
		{name: "oversized request stays asleep", requestedBytes: 1001, replaceableBytes: 100, replaceableSlots: 1, wantStatus: tasks.WaitingQuota},
		{name: "uncredited slot stays occupied", requestedBytes: 900, replaceableBytes: 100, wantStatus: tasks.WaitingQuota},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			gate, err := newWriteGate(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := tasks.NewManager(store, 1, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			waiter, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
			if err != nil {
				t.Fatal(err)
			}
			sessionID := mustGateTestID(t)
			if err := gate.AdmitProtection(ctx, sessionID, "direct-blob-scope", waiter.ID, 100, 4, 1000); err != nil {
				t.Fatal(err)
			}
			waiter.Status = tasks.WaitingQuota
			waiter.WaitReason = "distribution_quota"
			waiter.QuotaWaitAt = time.Now().UTC()
			waiter.QuotaWaitReason = "distribution_quota"
			waiter.QuotaWaitBytes = test.requestedBytes
			waiter.QuotaReplaceableBytes = test.replaceableBytes
			waiter.QuotaReplaceableSlots = test.replaceableSlots
			currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+waiter.ID, "quota-test", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.SaveProgress(ctx, &waiter, currentLease); err != nil {
				t.Fatal(err)
			}
			if err := manager.Leases().Release(ctx, currentLease); err != nil {
				t.Fatal(err)
			}

			service := &Service{
				tasks: manager,
				gate:  gate,
				cfg: config.Config{Solidification: config.Solidification{
					MaxProtectionSessions: 1, MaxUnanchoredDistributionBytes: 1000,
				}},
			}
			if err := service.wakeAvailableQuotaWaiter(ctx, "distribution_quota", gate.State()); err != nil {
				t.Fatal(err)
			}
			persisted, err := manager.Get(ctx, waiter.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.Status != test.wantStatus {
				t.Fatalf("waiter status = %q, want %q", persisted.Status, test.wantStatus)
			}
		})
	}
}
