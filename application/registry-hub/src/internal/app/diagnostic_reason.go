package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func diagnosticReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, inventory.ErrCurrentExpired), errors.Is(err, inventory.ErrExpired):
		return "expired"
	case errors.Is(err, storage.ErrNotFound), errors.Is(err, inventory.ErrNotObserved):
		return "not_observed"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, storage.ErrTimeout):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, storage.ErrCorruptObject):
		return "corrupt"
	case errors.Is(err, storage.ErrConditionFailed), errors.Is(err, storage.ErrLeaseLost):
		return "superseded"
	case errors.Is(err, storage.ErrPermissionDenied):
		return "permission_denied"
	default:
		return "storage_unavailable"
	}
}
