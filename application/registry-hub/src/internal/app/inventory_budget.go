package app

import (
	"context"
	"sync/atomic"
	"time"
)

type inventoryBudgetKey struct{}

type inventoryBudget struct {
	operations atomic.Uint64
}

type inventoryBatchProgress struct {
	StartedAt         time.Time `json:"startedAt"`
	Steps             uint64    `json:"steps"`
	StorageOperations uint64    `json:"storageOperationsBeforeCheckpoint"`
}

func inventoryBudgetContext(ctx context.Context) (context.Context, *inventoryBudget) {
	if budget, ok := ctx.Value(inventoryBudgetKey{}).(*inventoryBudget); ok {
		return ctx, budget
	}
	budget := &inventoryBudget{}
	return context.WithValue(ctx, inventoryBudgetKey{}, budget), budget
}
