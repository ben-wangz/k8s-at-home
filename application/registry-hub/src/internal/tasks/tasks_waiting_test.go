package tasks

import (
	"context"
	"testing"
	"time"

	"registry-hub/internal/storage/filesystem"
)

func TestWakeOldestWaitingQuotaUsesStableCreationOrder(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx := context.Background()
	base := time.Now().UTC()

	var waiting []Task
	for index, repo := range []string{"team/late", "team/early", "team/cancelled"} {
		task, _, err := manager.Enqueue(ctx, "ghcr.io", repo, "latest")
		if err != nil {
			t.Fatal(err)
		}
		task.Status = WaitingQuota
		task.WaitReason = "distribution_quota"
		task.CreatedAt = base.Add(time.Duration(index) * time.Second)
		task.QuotaWaitAt = base.Add(time.Duration(index) * time.Second)
		task.QuotaWaitReason = "distribution_quota"
		task.CancelRequested = repo == "team/cancelled"
		if err := manager.save(ctx, &task); err != nil {
			t.Fatal(err)
		}
		waiting = append(waiting, task)
	}
	waiting[0].QuotaWaitAt = base.Add(2 * time.Second)
	waiting[1].QuotaWaitAt = base
	for _, task := range waiting[:2] {
		if err := manager.save(ctx, &task); err != nil {
			t.Fatal(err)
		}
	}

	woke, err := manager.WakeOldestWaitingQuotaFor(ctx, "distribution_quota")
	if err != nil || !woke {
		t.Fatalf("WakeOldestWaitingQuotaFor() = %t, %v", woke, err)
	}
	first, err := manager.Get(ctx, waiting[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Get(ctx, waiting[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != Pending || second.Status != WaitingQuota {
		t.Fatalf("FIFO statuses = first %q, second %q; want pending then waiting", first.Status, second.Status)
	}
}

func TestWokenQuotaWaiterKeepsPriorityUntilAdmissionFinishes(t *testing.T) {
	for _, status := range []State{Pending, Running} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			manager, err := NewManager(store, 1, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			older, _, err := manager.Enqueue(ctx, "ghcr.io", "team/older-large", "latest")
			if err != nil {
				t.Fatal(err)
			}
			younger, _, err := manager.Enqueue(ctx, "ghcr.io", "team/younger-small", "latest")
			if err != nil {
				t.Fatal(err)
			}
			base := time.Now().UTC().Add(-time.Minute)
			for i, task := range []*Task{&older, &younger} {
				task.Status, task.WaitReason = WaitingQuota, "distribution_quota"
				task.QuotaWaitAt = base.Add(time.Duration(i) * time.Second)
				task.QuotaWaitReason = "distribution_quota"
				if err := manager.save(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			if woke, err := manager.WakeOldestWaitingQuota(ctx); err != nil || !woke {
				t.Fatalf("first wake = %t, %v", woke, err)
			}
			older, err = manager.Get(ctx, older.ID)
			if err != nil {
				t.Fatal(err)
			}
			older.Status = status
			if err := manager.save(ctx, &older); err != nil {
				t.Fatal(err)
			}
			if turn, err := manager.IsQuotaTurn(ctx, younger.ID, "distribution_quota"); err != nil || turn {
				t.Fatalf("younger admission while older is %s = %t, %v, want blocked", status, turn, err)
			}
			if woke, err := manager.WakeOldestWaitingQuota(ctx); err != nil || woke {
				t.Fatalf("second wake while older is %s = %t, %v, want no younger wake", status, woke, err)
			}
			clearTaskQuotaWait(&older)
			if err := manager.save(ctx, &older); err != nil {
				t.Fatal(err)
			}
			if woke, err := manager.WakeOldestWaitingQuota(ctx); err != nil || !woke {
				t.Fatalf("wake after older admission = %t, %v", woke, err)
			}
		})
	}
}
