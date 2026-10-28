package dnsrouter

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	Requests *prometheus.CounterVec
	Latency  *prometheus.HistogramVec
	Upstream *prometheus.CounterVec
	Cache    *prometheus.CounterVec
}

func NewMetrics(registry *prometheus.Registry) *Metrics {
	metrics := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "split_dns",
			Name:      "requests_total",
			Help:      "DNS requests by selected upstream group and result.",
		}, []string{"group", "rcode", "source"}),
		Latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "split_dns",
			Name:      "request_duration_seconds",
			Help:      "DNS request duration by selected upstream group.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"group"}),
		Upstream: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "split_dns",
			Name:      "upstream_attempts_total",
			Help:      "DNS upstream attempts by group, transport, and result.",
		}, []string{"group", "transport", "result"}),
		Cache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "split_dns",
			Name:      "cache_events_total",
			Help:      "DNS cache lookups by result.",
		}, []string{"result"}),
	}
	registry.MustRegister(metrics.Requests, metrics.Latency, metrics.Upstream, metrics.Cache)
	return metrics
}
