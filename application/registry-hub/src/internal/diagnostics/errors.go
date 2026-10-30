package diagnostics

import (
	"context"
	"errors"
	"net"
	"registry-hub/internal/storage"
	"strconv"
)

func SafeErrorText(cause error) string {
	if cause == nil {
		return ""
	}
	for _, item := range []struct {
		err  error
		text string
	}{
		{context.Canceled, "operation cancelled by context; responsibility unknown"},
		{context.DeadlineExceeded, "operation deadline exceeded; responsibility unknown"},
		{storage.ErrTimeout, "storage call timed out; responsibility unknown"},
		{storage.ErrPermissionDenied, "storage access denied"},
		{storage.ErrCorruptObject, "stored metadata or payload failed validation"},
		{storage.ErrLeaseLost, "operation lost its storage lease"},
		{storage.ErrConditionFailed, "operation conflicted with a newer stored revision"},
		{storage.ErrUnknownCommit, "storage write outcome is uncertain; recovery must verify it"},
		{storage.ErrNotFound, "required stored object was not found"},
		{storage.ErrUnavailable, "storage operation unavailable; responsibility unknown"},
	} {
		if errors.Is(cause, item.err) {
			return item.text
		}
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(cause, &status) {
		return "HTTP operation returned status " + strconv.Itoa(status.HTTPStatusCode())
	}
	var network net.Error
	if errors.As(cause, &network) && network.Timeout() {
		return "network operation timed out; responsibility unknown"
	}
	return "operation failed; underlying cause is not classified"
}
