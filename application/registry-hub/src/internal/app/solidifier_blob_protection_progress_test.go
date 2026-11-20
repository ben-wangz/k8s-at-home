package app

import (
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/tasks"
	"testing"
)

func TestBlobProtectionAdmissionRetryRequiresSameActiveAttempt(t *testing.T) {
	base := tasks.Task{ID: "blob-task", Kind: tasks.BlobKind, Status: tasks.Running, AttemptID: "attempt-a", RoundID: "round-a", Generation: 3, FencingToken: 8}
	currentLease := lease.Lease{FencingToken: 8}
	for _, test := range []struct {
		name    string
		mutate  func(*tasks.Task)
		wantErr error
	}{
		{name: "compatible", mutate: func(*tasks.Task) {}, wantErr: nil},
		{name: "cancelled", mutate: func(task *tasks.Task) { task.CancelRequested = true }, wantErr: tasks.ErrCancelled},
		{name: "replacement_attempt", mutate: func(task *tasks.Task) { task.AttemptID = "attempt-b" }, wantErr: lease.ErrLost},
		{name: "replacement_round", mutate: func(task *tasks.Task) { task.RoundID = "round-b" }, wantErr: lease.ErrLost},
		{name: "lost_fence", mutate: func(task *tasks.Task) { task.FencingToken++ }, wantErr: lease.ErrLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := base
			test.mutate(&current)
			err := blobProtectionAdmissionRetryError(base, current, currentLease)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("retry error = %v, want %v", err, test.wantErr)
			}
		})
	}
}
