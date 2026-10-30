package app

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"log/slog"
	"registry-hub/internal/storage"
	"time"
)

type metrics struct {
	backgroundAdmission    *prometheus.HistogramVec
	backgroundRemaining    *prometheus.HistogramVec
	inventory              *inventoryMetrics
	backgroundDuration     *prometheus.HistogramVec
	backgroundWait         *prometheus.HistogramVec
	registry               *prometheus.Registry
	requests               *prometheus.CounterVec
	requestLatency         *prometheus.HistogramVec
	upstream               *prometheus.CounterVec
	tasks                  *prometheus.GaugeVec
	taskKinds              *prometheus.GaugeVec
	taskAuthorities        *prometheus.GaugeVec
	taskErrors             *prometheus.GaugeVec
	taskWaitReasons        *prometheus.GaugeVec
	taskRetries            *prometheus.GaugeVec
	taskCancelPhases       *prometheus.GaugeVec
	blobIntents            *prometheus.GaugeVec
	blobIntentLimit        prometheus.Gauge
	taskRunnable           prometheus.Gauge
	taskWaiting            prometheus.Gauge
	recoveryUnknown        prometheus.Gauge
	gcBlockers             *prometheus.GaugeVec
	gate                   *prometheus.GaugeVec
	concurrency            *prometheus.GaugeVec
	rejections             *prometheus.CounterVec
	events                 *prometheus.CounterVec
	gcRuns                 *prometheus.CounterVec
	gcDuration             prometheus.Histogram
	storageOps             *prometheus.CounterVec
	storageLatency         *prometheus.HistogramVec
	storageWarnings        *prometheus.CounterVec
	objectLeases           *prometheus.GaugeVec
	cacheObjects           prometheus.Gauge
	cacheBytes             prometheus.Gauge
	cacheStaging           prometheus.Gauge
	cacheOrphans           prometheus.Gauge
	cacheCleanup           prometheus.Gauge
	distributionErr        *prometheus.CounterVec
	distributionVal        *prometheus.CounterVec
	referrerSources        *prometheus.CounterVec
	referrerRepair         prometheus.Gauge
	referrerReconcile      *prometheus.CounterVec
	protectionSessions     *prometheus.GaugeVec
	protectionReserved     *prometheus.GaugeVec
	protectionLimit        prometheus.Gauge
	protectedCacheReserved prometheus.Gauge
	protectedCacheLimit    prometheus.Gauge
	recoveryBarrier        prometheus.Gauge
	partialReserved        prometheus.Gauge
	partialSessions        prometheus.Gauge
	partialLimit           prometheus.Gauge
	s3ReplayMemory         prometheus.Gauge
	s3ReplayDisk           prometheus.Gauge
	s3ReplayActive         prometheus.Gauge
}

var taskWaitReasonLabels = []string{
	"awaiting_manifest_anchor", "awaiting_manifest_demand", "cache_entry_unavailable", "dependencies_unavailable",
	"distribution_quota", "gc_admission", "manifest_scope_admission", "protected_cache_capacity",
	"protection_transfer", "remote_operations_pending", "other",
}

var blobIntentMetricStates = []string{
	"waiting_cache", "waiting_capacity", "ready", "running", "retry_wait", "verified_local", "anchored", "failed", "cancelled", "other",
}

func (m *metrics) recordStorage(operation string, err error, duration time.Duration) string {
	result := storageResult(err)
	m.storageOps.WithLabelValues(operation, result).Inc()
	m.storageLatency.WithLabelValues(operation, result).Observe(duration.Seconds())
	if err != nil && !expectedStorageError(err) {
		m.storageWarnings.WithLabelValues(operation, result).Inc()
	}
	return result
}

func storageResult(err error) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, storage.ErrNotFound):
		return "not_found"
	case errors.Is(err, storage.ErrAlreadyExists):
		return "already_exists"
	case errors.Is(err, storage.ErrConditionFailed):
		return "condition_failed"
	case errors.Is(err, storage.ErrTimeout):
		return "timeout"
	case errors.Is(err, storage.ErrUnavailable):
		return "unavailable"
	case errors.Is(err, storage.ErrPermissionDenied):
		return "permission_denied"
	case errors.Is(err, storage.ErrCorruptObject):
		return "corrupt"
	case errors.Is(err, storage.ErrUnknownCommit):
		return "unknown_commit"
	case errors.Is(err, storage.ErrLeaseLost):
		return "lease_lost"
	case errors.Is(err, storage.ErrInvalidKey):
		return "invalid_key"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "error"
	}
}

func expectedStorageError(err error) bool {
	return errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, context.Canceled)
}

type observedStore struct {
	diagnosticFailure func(context.Context, string, error, time.Time)
	inventoryNotify   func()
	storage.Store
	metrics *metrics
	logger  *slog.Logger
}
