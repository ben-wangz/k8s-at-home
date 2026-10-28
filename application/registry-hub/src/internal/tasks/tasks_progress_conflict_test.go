package tasks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
	"time"
)

func TestSaveProgressConflictPreservesConcurrentStateAndDiagnostics(t *testing.T) {
	for _, stage := range []string{"version_check", "conditional_write"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			base, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer base.Close()
			injectConflict := false
			store := &taskCreateFaultStore{Store: base, put: func(_ context.Context, key string, _ io.Reader, _ int64, options storage.PutOptions) (storage.Entry, error, bool) {
				if injectConflict && strings.HasPrefix(key, "tasks/state/") {
					if options.IfMatch == "" {
						t.Fatal("task progress write omitted its version condition")
					}
					return storage.Entry{}, storage.ErrConditionFailed, true
				}
				return storage.Entry{}, nil, false
			}}
			manager, err := NewManager(store, 1, 10, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Close()
			task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
			if err != nil {
				t.Fatal(err)
			}
			currentLease, err := manager.Leases().Acquire(ctx, "tasks/"+task.ID, "conflict-regression", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.Leases().Release(ctx, currentLease)
			current := task
			if stage == "version_check" {
				current.Discovery = "complete"
				if err := manager.SaveProgress(ctx, &current, currentLease); err != nil {
					t.Fatal(err)
				}
			} else {
				injectConflict = true
			}
			err = manager.SaveProgress(ctx, &task, currentLease)
			if !errors.Is(err, storage.ErrConditionFailed) {
				t.Fatalf("SaveProgress() error = %v, want condition failure", err)
			}
			fields := []string{"task_id=" + task.ID, "stage=" + stage, fmt.Sprintf("expected_version=%q", task.version)}
			if stage == "version_check" {
				fields = append(fields, fmt.Sprintf("observed_version=%q", current.version))
			}
			for _, field := range fields {
				if !strings.Contains(err.Error(), field) {
					t.Errorf("conflict error %q omits %q", err, field)
				}
			}
			persisted, err := manager.Get(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if persisted.version != current.version || persisted.Discovery != current.Discovery {
				t.Fatalf("conflicting worker overwrote concurrent state: %#v", persisted)
			}
		})
	}
}
