package app

import (
	"github.com/prometheus/client_golang/prometheus"
	"time"
)

func (m *metrics) initBackgroundBudgetMetrics() {
	m.initPayloadMetrics()
	m.storageWorkOps = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_storage_work_operations_total", Help: "Storage calls attributed to bounded worker, purpose, key class, and reason labels."}, []string{"kind", "purpose", "operation", "key_class", "reason", "result"})
	m.storageWorkLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_storage_work_duration_seconds", Help: "Storage call durations by bounded worker, purpose, key class, and reason labels.", Buckets: prometheus.DefBuckets}, []string{"kind", "purpose", "operation", "key_class", "reason", "result"})
	m.s3HTTPAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_s3_http_attempts_total", Help: "Actual S3 HTTP attempts, including SDK retries, by bounded attribution labels."}, []string{"domain", "kind", "purpose", "operation", "key_class", "reason", "result"})
	m.s3HTTPLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_s3_http_attempt_duration_seconds", Help: "Duration of individual S3 HTTP attempts by bounded attribution labels.", Buckets: []float64{.005, .01, .05, .1, .25, .5, 1, 2, 5, 10, 30}}, []string{"domain", "kind", "purpose", "operation", "key_class", "reason", "result"})
	m.registryHTTPAttempts = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_registry_http_attempts_total", Help: "HTTP requests observed at the internal Distribution RoundTripper boundary."}, []string{"kind", "purpose", "method", "operation", "reason", "result"})
	m.registryHTTPLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_registry_http_attempt_duration_seconds", Help: "Duration of internal Distribution HTTP attempts observed at the RoundTripper boundary.", Buckets: []float64{.005, .01, .05, .1, .25, .5, 1, 2, 5, 10, 30}}, []string{"kind", "purpose", "method", "operation", "reason", "result"})
	m.backgroundScanRounds = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_background_scan_rounds_total", Help: "Ended source, statistics, maintenance, and inventory publication rounds by bounded worker kind and result."}, []string{"kind", "result"})
	snapshotLabels := []string{"kind", "purpose", "reason"}
	m.inventorySnapshotWrites = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_inventory_snapshot_object_writes_total", Help: "Successfully created immutable inventory snapshot objects."}, snapshotLabels)
	m.inventorySnapshotWriteBytes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_inventory_snapshot_object_write_bytes_total", Help: "Bytes in successfully created immutable inventory snapshot objects."}, snapshotLabels)
	m.registry.MustRegister(m.storageWorkOps, m.storageWorkLatency, m.s3HTTPAttempts, m.s3HTTPLatency, m.registryHTTPAttempts, m.registryHTTPLatency, m.backgroundScanRounds, m.inventorySnapshotWrites, m.inventorySnapshotWriteBytes)
	m.storageCollection = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_storage_collection_seconds", Help: "Physical collector operation and publication durations.", Buckets: []float64{.005, .05, .25, 1, 2, 8, 15, 60, 180, 600, 3600}}, []string{"operation", "domain"})
	m.storageListSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_storage_collection_list_entries", Help: "Bounded physical collector source page sizes.", Buckets: []float64{1, 2, 4, 8, 16, 32}}, []string{"domain"})
	m.registry.MustRegister(m.storageCollection, m.storageListSize)
	m.backgroundAdmission = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_admission_wait_seconds", Help: "Time spent waiting for background admission, separate from execution.", Buckets: prometheus.DefBuckets}, []string{"kind", "result"})
	m.backgroundRemaining = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_execution_deadline_remaining_seconds", Help: "Execution time available immediately after admission.", Buckets: []float64{0, .01, .1, 1, 5, 10, 15, 60}}, []string{"kind"})
	m.diagnosticWakeLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_diagnostic_source_wake_latency_seconds", Help: "Durable diagnostic source wake latency by bounded handoff phase.", Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2, 5, 10}}, []string{"phase"})
	m.registry.MustRegister(m.backgroundAdmission, m.backgroundRemaining, m.diagnosticWakeLatency)
}

func (m *metrics) recordDiagnosticWakeLatency(phase string, elapsed time.Duration) {
	if m == nil || m.diagnosticWakeLatency == nil {
		return
	}
	m.diagnosticWakeLatency.WithLabelValues(phase).Observe(max(0, elapsed.Seconds()))
}
