package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	got, err := NormalizeDomain("API.Example.ORG.")
	if err != nil || got != "api.example.org" {
		t.Fatalf("NormalizeDomain() = %q, %v", got, err)
	}
	for _, invalid := range []string{"", ".", "-bad.example", "bad..example", "unicode-\u00e9.example"} {
		if _, err := NormalizeDomain(invalid); err == nil {
			t.Errorf("NormalizeDomain(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingDocuments(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown field", data: `{"unsupported":true}`, want: "unknown field"},
		{name: "trailing object", data: `{} {}`, want: "one JSON object"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Load() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestValidateRejectsNormalizedDuplicateRules(t *testing.T) {
	cfg := validConfig()
	cfg.DNS.Rules = []Rule{
		{Match: "suffix", Domain: "Example.ORG", UpstreamGroup: "default"},
		{Match: "suffix", Domain: "example.org.", UpstreamGroup: "default"},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Validate() error = %v, want duplicate rule error", err)
	}
}

func validConfig() Config {
	var cfg Config
	cfg.ListenAddress = "0.0.0.0"
	cfg.ListenPort = 5353
	cfg.Metrics.Address = "0.0.0.0"
	cfg.Metrics.Port = 8080
	cfg.DNS.DefaultUpstreamGroup = "default"
	cfg.DNS.UpstreamGroups = map[string]UpstreamGroup{
		"default": {Type: "forward", Servers: []string{"192.0.2.53:53"}, QueryTimeout: "2s", MaxConcurrentRequests: 4},
	}
	cfg.DNS.Proxies = map[string]Proxy{}
	return cfg
}
