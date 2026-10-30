package app

import "github.com/prometheus/client_golang/prometheus"

func (m *metrics) initBackgroundBudgetMetrics() {
	m.backgroundAdmission = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_admission_wait_seconds", Help: "Time spent waiting for background admission, separate from execution.", Buckets: prometheus.DefBuckets}, []string{"kind", "result"})
	m.backgroundRemaining = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_execution_deadline_remaining_seconds", Help: "Execution time available immediately after admission.", Buckets: []float64{0, .01, .1, 1, 5, 10, 15, 60}}, []string{"kind"})
	m.registry.MustRegister(m.backgroundAdmission, m.backgroundRemaining)
}
