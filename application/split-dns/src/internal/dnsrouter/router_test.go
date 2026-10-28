package dnsrouter

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	"split-dns/internal/config"
)

func TestSelectGroupBoundariesAndSpecificity(t *testing.T) {
	cfg := baseConfig()
	cfg.DNS.UpstreamGroups["specific"] = forwardGroup()
	cfg.DNS.UpstreamGroups["exact"] = forwardGroup()
	cfg.DNS.Rules = []config.Rule{
		{Match: "suffix", Domain: "example.com", UpstreamGroup: "specific"},
		{Match: "suffix", Domain: "foo.example.com", UpstreamGroup: "specific"},
		{Match: "exact", Domain: "foo.example.com", UpstreamGroup: "exact"},
	}
	router := newRouter(t, cfg)
	tests := map[string]string{
		"example.com":       "specific",
		"a.example.com":     "specific",
		"foo.example.com":   "exact",
		"x.foo.example.com": "specific",
		"notexample.com":    "default",
	}
	for name, want := range tests {
		if got := router.selectGroup(name); got != want {
			t.Errorf("selectGroup(%q) = %q, want %q", name, got, want)
		}
	}
}

func baseConfig() config.Config {
	var cfg config.Config
	cfg.ListenAddress = "0.0.0.0"
	cfg.ListenPort = 5353
	cfg.Metrics.Address = "0.0.0.0"
	cfg.Metrics.Port = 8080
	cfg.DNS.DefaultUpstreamGroup = "default"
	cfg.DNS.UpstreamGroups = map[string]config.UpstreamGroup{"default": forwardGroup()}
	cfg.DNS.Proxies = map[string]config.Proxy{}
	return cfg
}

func forwardGroup() config.UpstreamGroup {
	return config.UpstreamGroup{Type: "forward", Servers: []string{"192.0.2.53:53"}, QueryTimeout: "2s", MaxConcurrentRequests: 8}
}

func newRouter(t *testing.T, cfg config.Config) *Router {
	t.Helper()
	metrics := NewMetrics(prometheus.NewRegistry())
	router, err := New(cfg, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func query(name string) *dns.Msg {
	request := new(dns.Msg)
	request.SetQuestion(name, dns.TypeA)
	request.Id = 1245
	return request
}
