package app

import "registry-hub/internal/distribution"

func (m *metrics) recordRegistryHTTPAttempt(attempt distribution.HTTPAttempt) {
	if m == nil || m.registryHTTPAttempts == nil {
		return
	}
	result := distribution.HTTPAttemptResult(attempt)
	kind, purpose := storageWorkLabels(attempt.Context)
	labels := []string{kind, purpose, attempt.Method, attempt.Operation, storageReasonLabel(attempt.Context), result}
	m.registryHTTPAttempts.WithLabelValues(labels...).Inc()
	duration := attempt.Duration.Seconds()
	if duration < 0 {
		duration = 0
	}
	m.registryHTTPLatency.WithLabelValues(labels...).Observe(duration)
}
