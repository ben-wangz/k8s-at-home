package app

import (
	"context"
	"errors"
	"github.com/prometheus/client_golang/prometheus"
	"io"
	"log/slog"
	"registry-hub/internal/storage"
	"time"
)

type metrics struct {
	registry          *prometheus.Registry
	requests          *prometheus.CounterVec
	requestLatency    *prometheus.HistogramVec
	upstream          *prometheus.CounterVec
	tasks             *prometheus.GaugeVec
	taskAuthorities   *prometheus.GaugeVec
	taskErrors        *prometheus.GaugeVec
	gate              *prometheus.GaugeVec
	concurrency       *prometheus.GaugeVec
	rejections        *prometheus.CounterVec
	events            *prometheus.CounterVec
	gcRuns            *prometheus.CounterVec
	gcDuration        prometheus.Histogram
	storageOps        *prometheus.CounterVec
	storageLatency    *prometheus.HistogramVec
	storageWarnings   *prometheus.CounterVec
	objectLeases      *prometheus.GaugeVec
	cacheObjects      prometheus.Gauge
	cacheBytes        prometheus.Gauge
	cacheStaging      prometheus.Gauge
	cacheOrphans      prometheus.Gauge
	cacheCleanup      prometheus.Gauge
	distributionErr   *prometheus.CounterVec
	distributionVal   *prometheus.CounterVec
	referrerSources   *prometheus.CounterVec
	referrerRepair    prometheus.Gauge
	referrerReconcile *prometheus.CounterVec
}

func newMetrics() *metrics {
	result := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_requests_total", Help: "Client Registry requests.",
		}, []string{"operation", "method", "status", "cache_source", "error_class"}),
		requestLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "registry_hub_request_duration_seconds", Help: "Client Registry request latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "method", "status", "cache_source", "error_class"}),
		upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_upstream_requests_total", Help: "Public Registry request outcomes.",
		}, []string{"result"}),
		tasks: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_tasks", Help: "Persisted solidification tasks by state.",
		}, []string{"state"}),
		taskAuthorities: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_tasks_by_authority_class", Help: "Persisted solidification tasks by state and authority class.",
		}, []string{"state", "authority_class"}),
		taskErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_task_errors", Help: "Persisted solidification tasks by stable error class.",
		}, []string{"class"}),
		gate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_gc_gate", Help: "Whether the GC write gate is in a named state.",
		}, []string{"state"}),
		concurrency: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_concurrency_slots", Help: "Currently occupied process-local concurrency slots.",
		}, []string{"gate"}),
		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_rejections_total", Help: "Requests rejected by a rate or concurrency gate.",
		}, []string{"gate"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_events_total", Help: "Low-cardinality Registry Hub events.",
		}, []string{"event"}),
		gcRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_gc_runs_total", Help: "Distribution GC results.",
		}, []string{"result"}),
		gcDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "registry_hub_gc_duration_seconds", Help: "Distribution GC execution duration.",
			Buckets: prometheus.DefBuckets,
		}),
		storageOps: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_storage_operations_total", Help: "Storage operations by result.",
		}, []string{"operation", "result"}),
		storageLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "registry_hub_storage_operation_duration_seconds", Help: "Storage operation latency.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation", "result"}),
		storageWarnings: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_storage_warnings_total", Help: "Unexpected storage failures that were logged as warnings.",
		}, []string{"operation", "result"}),
		objectLeases: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "registry_hub_object_leases", Help: "Object download leases by local state.",
		}, []string{"state"}),
		cacheObjects: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_objects", Help: "Verified cached content objects.",
		}),
		cacheBytes: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_bytes", Help: "Bytes held by verified cached content objects.",
		}),
		cacheStaging: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_staging_objects", Help: "Recent cache objects without a complete commit record.",
		}),
		cacheOrphans: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_orphan_objects", Help: "Stale cache objects without a complete commit record.",
		}),
		cacheCleanup: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_cache_cleanup_pending_objects", Help: "Verified cache objects pending cleanup.",
		}),
		distributionErr: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_distribution_api_errors_total", Help: "Internal Distribution API errors by bounded result.",
		}, []string{"result"}),
		distributionVal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_distribution_response_validation_failures_total", Help: "Invalid internal Distribution responses by bounded validation class.",
		}, []string{"class"}),
		referrerSources: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_referrer_reads_total", Help: "Referrer reads by authoritative source.",
		}, []string{"source"}),
		referrerRepair: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "registry_hub_referrer_repair_pending", Help: "Whether fallback referrer repair work is pending.",
		}),
		referrerReconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "registry_hub_referrer_reconcile_total", Help: "Referrer reconciliation outcomes.",
		}, []string{"result"}),
	}
	result.registry.MustRegister(result.requests, result.requestLatency, result.upstream, result.tasks, result.taskAuthorities, result.taskErrors, result.gate, result.concurrency, result.rejections, result.events, result.gcRuns, result.gcDuration, result.storageOps, result.storageLatency, result.storageWarnings, result.objectLeases, result.cacheObjects, result.cacheBytes, result.cacheStaging, result.cacheOrphans, result.cacheCleanup, result.distributionErr, result.distributionVal, result.referrerSources, result.referrerRepair, result.referrerReconcile)
	result.objectLeases.WithLabelValues("active").Set(0)
	result.objectLeases.WithLabelValues("waiting").Set(0)
	result.referrerRepair.Set(0)
	for _, class := range []string{"headers", "manifest", "list", "referrers", "upload", "other"} {
		result.distributionVal.WithLabelValues(class).Add(0)
	}
	for _, event := range []string{"upstream_retry", "upstream_404", "upstream_fetch", "upstream_auth_challenge", "upstream_redirect", "local_hit", "digest_mismatch", "lease_lost", "download_wait_timeout"} {
		result.events.WithLabelValues(event).Add(0)
	}
	return result
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
	storage.Store
	metrics *metrics
	logger  *slog.Logger
}

func newObservedStore(store storage.Store, metrics *metrics, logger *slog.Logger) *observedStore {
	return &observedStore{Store: store, metrics: metrics, logger: logger}
}

func (s *observedStore) Get(ctx context.Context, key string) (storage.Object, error) {
	started := time.Now()
	object, err := s.Store.Get(ctx, key)
	s.record("get", err, started)
	return object, err
}

func (s *observedStore) Head(ctx context.Context, key string) (storage.Entry, error) {
	started := time.Now()
	entry, err := s.Store.Head(ctx, key)
	s.record("head", err, started)
	return entry, err
}

func (s *observedStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	started := time.Now()
	entry, err := s.Store.Put(ctx, key, body, size, options)
	s.record("put", err, started)
	return entry, err
}

func (s *observedStore) Delete(ctx context.Context, key string) error {
	started := time.Now()
	err := s.Store.Delete(ctx, key)
	s.record("delete", err, started)
	return err
}

func (s *observedStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	started := time.Now()
	err := s.Store.DeleteIfMatch(ctx, key, version)
	s.record("delete_if_match", err, started)
	return err
}

func (s *observedStore) List(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	started := time.Now()
	entries, next, err := s.Store.List(ctx, prefix, token, limit)
	s.record("list", err, started)
	return entries, next, err
}

func (s *observedStore) Close() error {
	started := time.Now()
	err := s.Store.Close()
	s.record("close", err, started)
	return err
}
