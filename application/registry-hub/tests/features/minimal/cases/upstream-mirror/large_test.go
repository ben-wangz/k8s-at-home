package upstreammirror

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
)

func TestLargeLayerReadRejectsWrongSizeDigestAndStatus(t *testing.T) {
	for _, scenario := range []struct {
		name string
		body string
		code int
		pass bool
	}{
		{"valid", "fixture", http.StatusOK, true},
		{"truncated", "fixtur", http.StatusOK, false},
		{"excess", "fixtures", http.StatusOK, false},
		{"corrupt", "FIXTURE", http.StatusOK, false},
		{"failure", "fixture", http.StatusServiceUnavailable, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(scenario.code)
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer server.Close()
			client := &support.RegistryClient{Base: server.URL, Client: server.Client()}
			_, err := readLargeLayer(context.Background(), client, "fixture", support.Descriptor{Digest: support.Digest([]byte("fixture")), Size: 7})
			if (err == nil) != scenario.pass {
				t.Fatalf("pass=%t error=%v", scenario.pass, err)
			}
		})
	}
}

func TestLargeLayerPerformanceMetadataIsExplicitAndBounded(t *testing.T) {
	item, err := NewLargeLayerPerformance()
	if err != nil {
		t.Fatal(err)
	}
	if item.Definition.ID != "large-layer-performance" || !strings.Contains(string(item.Raw), "1024 MiB") {
		t.Fatal("large-layer case lost its explicit acceptance sizes")
	}
}

func TestLargeBlobCounterOnlyIncludesSuccessfulReadsFromExpectedSource(t *testing.T) {
	body := `registry_hub_requests_total{operation="blob",method="GET",status="OK",cache_source="cache"} 2
registry_hub_requests_total{operation="blob",method="HEAD",status="OK",cache_source="cache"} 5
registry_hub_requests_total{operation="manifest",method="GET",status="OK",cache_source="cache"} 6
registry_hub_requests_total{operation="blob",method="GET",status="Not Found",cache_source="cache"} 7
registry_hub_requests_total{operation="blob",method="GET",status="OK",cache_source="local"} 8
`
	if count, err := largeBlobReadCounter(body, "cache"); err != nil || count != 2 {
		t.Fatalf("count=%v err=%v", count, err)
	}
	if _, err := largeBlobReadCounter(`registry_hub_requests_total{operation="blob",method="GET",status="OK",cache_source="cache"} invalid`, "cache"); err == nil {
		t.Fatal("invalid metric accepted")
	}
}
