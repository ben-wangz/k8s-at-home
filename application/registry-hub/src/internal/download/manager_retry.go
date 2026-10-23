package download

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

func strongETag(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(strings.ToLower(value), "w/") || len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return ""
	}
	return value
}

func retryable(err error) bool {
	if errors.Is(err, errProgressCommit) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
}

func (m *Manager) waitRetry(ctx context.Context, request Request, retryAfter string, attempt int) error {
	if request.WaitRetry == nil {
		return nil
	}
	return request.WaitRetry(ctx, retryAfter, attempt)
}

func terminalDownloadError(err error) bool {
	var persistence *PersistenceError
	var guard *GuardError
	return errors.Is(err, errProgressCommit) || errors.Is(err, ErrQuotaExceeded) ||
		errors.As(err, &persistence) || errors.As(err, &guard)
}

func (m *Manager) retryable(ctx context.Context, request Request, err error) bool {
	if ctx.Err() != nil || terminalDownloadError(err) {
		return false
	}
	if retryable(err) {
		return true
	}
	if request.Retryable != nil {
		return request.Retryable(err)
	}
	return false
}
