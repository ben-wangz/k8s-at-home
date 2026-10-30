package app

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"net/http"
	"registry-hub/internal/artifact"
	"strconv"
	"time"
)

func (s *observedStore) CleanupOrphans(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	cleaner, ok := s.Store.(interface {
		CleanupOrphans(context.Context, time.Duration, int) (int, error)
	})
	if !ok {
		err := errors.New("storage backend does not support orphan cleanup")
		s.record(ctx, "cleanup_orphans", err, time.Now())
		return 0, err
	}
	started := time.Now()
	removed, err := cleaner.CleanupOrphans(ctx, olderThan, limit)
	s.record(ctx, "cleanup_orphans", err, started)
	return removed, err
}

func (s *observedStore) CleanupOrphansStep(ctx context.Context, olderThan time.Duration, token string) (string, int, error) {
	cleaner, ok := s.Store.(interface {
		CleanupOrphansStep(context.Context, time.Duration, string) (string, int, error)
	})
	if !ok {
		return "", 0, nil
	}
	started := time.Now()
	next, removed, err := cleaner.CleanupOrphansStep(ctx, olderThan, token)
	s.record(ctx, "cleanup_orphans", err, started)
	return next, removed, err
}

func (s *observedStore) record(ctx context.Context, operation string, err error, started time.Time) {
	if err != nil && !expectedStorageError(err) && s.diagnosticFailure != nil {
		s.diagnosticFailure(ctx, operation, err, started)
	}
	if budget, ok := ctx.Value(inventoryBudgetKey{}).(*inventoryBudget); ok {
		budget.operations.Add(1)
	}
	result := s.metrics.recordStorage(operation, err, time.Since(started))
	if err != nil && !expectedStorageError(err) && s.logger != nil {
		fields := []any{"operation", operation, "error_class", result}
		if meta, ok := ctx.Value(requestMetaKey{}).(*requestMeta); ok && meta.requestID != "" {
			fields = append(fields, "request_id", meta.requestID)
		}
		if operationID, ok := ctx.Value(storageOperationIDKey{}).(string); ok && operationID != "" {
			fields = append(fields, "operation_id", operationID)
		}
		var requestID interface{ S3RequestID() string }
		if errors.As(err, &requestID) && requestID.S3RequestID() != "" {
			fields = append(fields, "s3_request_id", requestID.S3RequestID())
		}
		s.logger.Warn("storage operation failed", fields...)
	}
}

func (m *metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *metrics) recordRequest(operation, method string, status int, cacheSource, errorClass string, duration time.Duration) {
	labels := []string{operation, method, http.StatusText(status), cacheSource, boundedRequestErrorClass(errorClass, status)}
	m.requests.WithLabelValues(labels...).Inc()
	m.requestLatency.WithLabelValues(labels...).Observe(duration.Seconds())
	if cacheSource == "local" || cacheSource == "cache" {
		m.recordEvent("local_hit")
	}
}

func boundedRequestErrorClass(class string, status int) string {
	switch class {
	case "rate_limit", "concurrency", "authentication", "authorization", "queue", "storage", "upstream", "upstream_auth", "upstream_policy", "upstream_not_found", "object_wait_timeout", "manifest_invalid", "manifest_negotiation", "digest_mismatch", "fencing", "distribution", "referrer_index":
		return class
	case "":
		if status >= 500 {
			return "server_error"
		}
		if status >= 400 {
			return "client_error"
		}
		return "none"
	default:
		return "other"
	}
}

func (m *metrics) setCacheStats(stats artifact.CacheStats) {
	m.cacheObjects.Set(float64(stats.Objects))
	m.cacheBytes.Set(float64(stats.Bytes))
	m.cacheStaging.Set(float64(stats.Staging))
	m.cacheOrphans.Set(float64(stats.Orphans))
	m.cacheCleanup.Set(float64(stats.CleanupPending))
}

func (m *metrics) recordDistributionError(result string) {
	if m == nil || m.distributionErr == nil {
		return
	}
	if result != "transport_error" {
		status, err := strconv.Atoi(result)
		if err != nil || status < 400 || status > 599 {
			result = "other"
		} else {
			result = strconv.Itoa(status)
		}
	}
	m.distributionErr.WithLabelValues(result).Inc()
}

func (m *metrics) recordDistributionValidation(class string) {
	if m == nil || m.distributionVal == nil {
		return
	}
	switch class {
	case "location", "headers", "manifest", "list", "referrers", "upload":
		m.distributionVal.WithLabelValues(class).Inc()
	default:
		m.distributionVal.WithLabelValues("other").Inc()
	}
}

func (m *metrics) setReferrerRepairPending(pending bool) {
	if pending {
		m.referrerRepair.Set(1)
		return
	}
	m.referrerRepair.Set(0)
}

func (m *metrics) recordReferrerReconcile(result string) {
	if m == nil || m.referrerReconcile == nil {
		return
	}
	if result != "success" {
		result = "failure"
	}
	m.referrerReconcile.WithLabelValues(result).Inc()
}

func (m *metrics) recordReferrerSource(source string) {
	if m == nil || m.referrerSources == nil {
		return
	}
	if source != "native" && source != "fallback" {
		source = "other"
	}
	m.referrerSources.WithLabelValues(source).Inc()
}

func (m *metrics) setGateState(state string) {
	m.gate.Reset()
	m.gate.WithLabelValues(state).Set(1)
}

func (m *metrics) setConcurrency(gate string, occupied int) {
	m.concurrency.WithLabelValues(gate).Set(float64(occupied))
}

func (m *metrics) recordRejection(gate string) {
	m.rejections.WithLabelValues(gate).Inc()
}

func (m *metrics) recordEvent(event string) {
	m.events.WithLabelValues(event).Inc()
}
