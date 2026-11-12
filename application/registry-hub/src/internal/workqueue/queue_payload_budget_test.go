package workqueue

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"testing"
	"time"
)

func TestPayloadClaimCoversExecutionBudgetWithoutAllowingTakeover(t *testing.T) {
	q := queueFixture(t)
	ctx := context.Background()
	job, err := q.Enqueue(ctx, "cache_verification", "payload", "identity", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := q.ClaimFor(ctx, job, 5*time.Minute)
	if err != nil || time.Until(claimed.LeaseUntil) < 4*time.Minute {
		t.Fatalf("claim=%+v error=%v", claimed, err)
	}
	if _, err := q.ClaimFor(ctx, claimed, 5*time.Minute); !errors.Is(err, storage.ErrConditionFailed) {
		t.Fatalf("live payload claim was taken over: %v", err)
	}
	if err := q.Validate(ctx, claimed); err != nil {
		t.Fatal(err)
	}
}
