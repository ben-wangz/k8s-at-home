package app

import (
	"registry-hub/internal/inventory"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type inventoryMetrics struct {
	images        *prometheus.GaugeVec
	lastPublished prometheus.Gauge
	observed      prometheus.Gauge
	coverage      prometheus.Gauge
	batch         prometheus.Histogram
}

func (m *metrics) initInventoryMetrics() {
	m.inventory = &inventoryMetrics{
		images:        prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "registry_hub_inventory_images", Help: "Image counts in the published snapshot by scope and category."}, []string{"scope", "category"}),
		lastPublished: prometheus.NewGauge(prometheus.GaugeOpts{Name: "registry_hub_inventory_last_publish_timestamp_seconds", Help: "Unix time of the last successful inventory snapshot publication."}),
		observed:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "registry_hub_inventory_observed_timestamp_seconds", Help: "Unix time of the oldest observation boundary in the published inventory snapshot."}),
		coverage:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "registry_hub_inventory_coverage_complete", Help: "Whether all declared discoverable inventory sources completed their scan."}),
		batch:         prometheus.NewHistogram(prometheus.HistogramOpts{Name: "registry_hub_inventory_batch_duration_seconds", Help: "Inventory background batch duration including storage and local registry checks.", Buckets: prometheus.DefBuckets}),
	}
	i := m.inventory
	m.registry.MustRegister(i.images, i.lastPublished, i.observed, i.coverage, i.batch)
}

func (m *metrics) inventoryPublished(snapshot inventory.Snapshot) {
	if m == nil || m.inventory == nil {
		return
	}
	i := m.inventory
	i.images.Reset()
	for key, count := range snapshot.Counts {
		scope, category, ok := strings.Cut(key, ":")
		if ok {
			i.images.WithLabelValues(scope, category).Set(float64(count))
		}
	}
	i.lastPublished.Set(float64(time.Now().Unix()))
	i.observed.Set(float64(snapshot.ObservedAt.Unix()))
	i.coverage.Set(0)
	if snapshot.Coverage == "complete" {
		i.coverage.Set(1)
	}
}
