package app

import (
	"context"
	"errors"
	"strings"
	"sync"
)

func retentionCleanupKind(kind string) bool {
	return kind == historyCleanupWorkKind || kind == historyCleanupPreviewKind || kind == "receipt_cleanup" ||
		kind == "receipt_cleanup_preview" || kind == "retry_request_retention" || strings.HasPrefix(kind, "diagnostic_retention_")
}

var errHistoryCleanupRequestBudget = errors.New("history cleanup storage request budget reached")

type historyCleanupRequestBudgetKey struct{}
type historyCleanupCheckpointRequestKey struct{}

type historyCleanupRequestBudget struct {
	mu        sync.Mutex
	remaining int
}

func withHistoryCleanupRequestBudget(ctx context.Context, limit int) context.Context {
	return context.WithValue(ctx, historyCleanupRequestBudgetKey{}, &historyCleanupRequestBudget{remaining: limit})
}

func withHistoryCleanupCheckpointBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, historyCleanupCheckpointRequestKey{}, true)
}

func consumeHistoryCleanupStorageRequest(ctx context.Context) error {
	budget, ok := ctx.Value(historyCleanupRequestBudgetKey{}).(*historyCleanupRequestBudget)
	if !ok {
		return nil
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	reserve := 0
	if checkpoint, _ := ctx.Value(historyCleanupCheckpointRequestKey{}).(bool); !checkpoint {
		reserve = 16
	}
	if budget.remaining <= reserve {
		return errHistoryCleanupRequestBudget
	}
	budget.remaining--
	return nil
}
