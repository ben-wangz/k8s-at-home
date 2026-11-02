package app

import "github.com/prometheus/client_golang/prometheus"

func (m *metrics) initBackgroundBudgetMetrics() {
	m.storageCollection = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_storage_collection_seconds", Help: "Physical collector operation and publication durations.", Buckets: []float64{.005, .05, .25, 1, 2, 8, 15, 60, 180, 600, 3600}}, []string{"operation", "domain"})
	m.storageListSize = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_storage_collection_list_entries", Help: "Bounded physical collector source page sizes.", Buckets: []float64{1, 2, 4, 8, 16, 32}}, []string{"domain"})
	m.registry.MustRegister(m.storageCollection, m.storageListSize)
	m.backgroundAdmission = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_admission_wait_seconds", Help: "Time spent waiting for background admission, separate from execution.", Buckets: prometheus.DefBuckets}, []string{"kind", "result"})
	m.backgroundRemaining = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "registry_hub_background_execution_deadline_remaining_seconds", Help: "Execution time available immediately after admission.", Buckets: []float64{0, .01, .1, 1, 5, 10, 15, 60}}, []string{"kind"})
	m.registry.MustRegister(m.backgroundAdmission, m.backgroundRemaining)
}
