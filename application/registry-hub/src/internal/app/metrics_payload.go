package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func (m *metrics) initPayloadMetrics() {
	labels := []string{"kind", "purpose", "operation"}
	m.payloadBytes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_payload_bytes_total", Help: "Consumed bytes read, written, or hashed; includes filesystem version hashing."}, labels)
	m.payloadDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_payload_duration_seconds", Help: "Active payload I/O and verification durations.", Buckets: []float64{.001, .01, .1, 1, 5, 15, 60, 300, 1800}}, labels)
	m.verificationOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "registry_hub_distribution_verification_total", Help: "Distribution blob verification decisions."}, []string{"result"})
	m.verificationDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_distribution_verification_seconds", Help: "Time spent on an individual Distribution blob verification decision.", Buckets: []float64{.001, .01, .1, 1, 5, 15, 60, 300, 1800}}, []string{"result"})
	m.registry.MustRegister(m.payloadBytes, m.payloadDuration, m.verificationOutcomes, m.verificationDuration)
}

func (m *metrics) recordDistributionVerification(result string, started time.Time) {
	if m == nil || m.verificationOutcomes == nil {
		return
	}
	switch result {
	case "reuse", "readback", "wait", "error":
	default:
		result = "error"
	}
	m.verificationOutcomes.WithLabelValues(result).Inc()
	m.verificationDuration.WithLabelValues(result).Observe(time.Since(started).Seconds())
}

func (m *metrics) recordPayload(ctx context.Context, operation string, count int64, elapsed time.Duration) {
	if m == nil || m.payloadBytes == nil {
		return
	}
	kind, _ := storageWorkLabels(ctx)
	purpose := storage.IOPurpose(ctx)
	switch purpose {
	case "cache_verification", "client_delivery", "distribution_upload", "distribution_readback", "download_assembly", "cleanup_verification":
	default:
		purpose = "storage"
	}
	switch operation {
	case "read", "write", "hash", "s3_read", "s3_write":
	default:
		operation = "other"
	}
	m.payloadBytes.WithLabelValues(kind, purpose, operation).Add(float64(count))
	m.payloadDuration.WithLabelValues(kind, purpose, operation).Observe(elapsed.Seconds())
}

func (s *observedStore) ObserveIO(ctx context.Context, operation string, count int64, elapsed time.Duration) {
	s.metrics.recordPayload(ctx, operation, count, elapsed)
}

func (s *observedStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	source, ok := s.Store.(storage.PayloadOpener)
	if !ok {
		return storage.Payload{}, errors.ErrUnsupported
	}
	started := time.Now()
	payload, err := source.OpenPayload(storage.WithIOObserver(ctx, s.ObserveIO), key)
	if errors.Is(err, errors.ErrUnsupported) {
		return payload, err
	}
	s.record(ctx, "open_payload", err, started)
	return payload, err
}

func (s *observedStore) GetIfMatch(ctx context.Context, key, version string) (storage.Object, error) {
	source, ok := s.Store.(storage.ConditionalGetter)
	if !ok {
		return storage.Object{}, errors.ErrUnsupported
	}
	started := time.Now()
	ioCtx := storage.WithIOObserver(ctx, s.ObserveIO)
	object, err := source.GetIfMatch(ioCtx, key, version)
	if err == nil {
		object.Body = storage.MeasureBody(ioCtx, object.Body)
	}
	recordInventoryDependency(ctx, key, object.Version, err)
	s.record(ctx, "get", err, started)
	return object, err
}
