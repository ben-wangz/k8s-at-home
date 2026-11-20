package testkit

import "testing"

func TestPrometheusSumFiltersLabelsAndDecodesEscapes(t *testing.T) {
	metrics := []byte("" +
		"registry_hub_s3_http_attempts_total{domain=\"state\",reason=\"inventory,\"} 2\n" +
		"registry_hub_s3_http_attempts_total{domain=\"registry\",reason=\"inventory\"} 3\n" +
		"registry_hub_s3_http_attempts_total{domain=\"state\",reason=\"inventory\\\"build\"} 5\n")
	total, found, err := PrometheusSum(metrics, "registry_hub_s3_http_attempts_total", map[string]string{"domain": "state"})
	if err != nil || !found || total != 7 {
		t.Fatalf("PrometheusSum() = (%v, %v, %v), want (7, true, nil)", total, found, err)
	}
}

func TestPrometheusSumReportsMissingSeries(t *testing.T) {
	total, found, err := PrometheusSum([]byte("registry_hub_requests_total 1\n"), "registry_hub_s3_http_attempts_total", nil)
	if err != nil || found || total != 0 {
		t.Fatalf("PrometheusSum() = (%v, %v, %v), want (0, false, nil)", total, found, err)
	}
}

func TestPrometheusSumHandlesSpacesInsideLabelValues(t *testing.T) {
	metrics := []byte("registry_hub_events_total{reason=\"inventory refresh\",kind=\"inventory_builder\"} 4\n")
	total, found, err := PrometheusSum(metrics, "registry_hub_events_total", map[string]string{"reason": "inventory refresh"})
	if err != nil || !found || total != 4 {
		t.Fatalf("PrometheusSum() = (%v, %v, %v), want (4, true, nil)", total, found, err)
	}
}

func TestPrometheusSamplesPreserveAllLabels(t *testing.T) {
	metrics := []byte("registry_hub_storage_work_operations_total{kind=\"inventory_builder\",reason=\"inventory refresh\",operation=\"get\"} 4\n")
	samples, err := PrometheusSamples(metrics, "registry_hub_storage_work_operations_total")
	if err != nil || len(samples) != 1 {
		t.Fatalf("PrometheusSamples() = (%v, %v), want one sample", samples, err)
	}
	if samples[0].Labels["reason"] != "inventory refresh" || samples[0].Labels["operation"] != "get" || samples[0].Value != 4 {
		t.Fatalf("PrometheusSamples() = %+v, want labels and value preserved", samples[0])
	}
}
