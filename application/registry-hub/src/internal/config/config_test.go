package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadUsesDefaultClosureLimits(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["pull"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load default configuration: %v", err)
	}
	if loaded.Limits.ObjectBytes != 100<<30 || loaded.Limits.ClosureObjects != 10000 || loaded.Limits.ClosureBytes != 1<<40 || loaded.Limits.ClosureDepth != 64 || loaded.Limits.RequestTimeout != 300*time.Second || loaded.Limits.BlobTransferTimeout != 24*time.Hour || loaded.Limits.UpstreamBodyIdleTimeout != 60*time.Second || loaded.Limits.DownloadPartialTTL != 24*time.Hour || loaded.Limits.DownloadMaxBytes != 200<<30 {
		t.Fatalf("default limits = object_bytes:%d objects:%d bytes:%d depth:%d request_timeout:%s blob_timeout:%s idle_timeout:%s partial_ttl:%s download_bytes:%d", loaded.Limits.ObjectBytes, loaded.Limits.ClosureObjects, loaded.Limits.ClosureBytes, loaded.Limits.ClosureDepth, loaded.Limits.RequestTimeout, loaded.Limits.BlobTransferTimeout, loaded.Limits.UpstreamBodyIdleTimeout, loaded.Limits.DownloadPartialTTL, loaded.Limits.DownloadMaxBytes)
	}
}

func TestLoadRejectsInvalidConfiguredClientAuth(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		required bool
	}{
		{name: "configured Secret is empty", required: true},
		{name: "empty object", value: `{}`},
		{name: "malformed JSON", value: `{"basic":`},
		{name: "invalid permissions", value: `{"basic":[{"username":"reader","password":"secret","permissions":["unknown"]}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearRegistryHubEnvironment(t)
			t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
			t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
			t.Setenv("REGISTRY_HUB_CLIENT_AUTH_CONFIG", test.value)
			if test.required {
				t.Setenv("REGISTRY_HUB_CLIENT_AUTH_CONFIG_REQUIRED", "true")
			}
			if _, err := Load(); err == nil {
				t.Fatal("Load accepted invalid configured client auth and could start no-auth")
			}
		})
	}
}

func TestLoadRejectsDownloadLimitAndTimeoutOverflow(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{name: "default quota multiplication", key: "REGISTRY_HUB_LIMIT_OBJECT_BYTES", value: "9223372036854775807"},
		{name: "blob timeout duration", key: "REGISTRY_HUB_BLOB_TRANSFER_TIMEOUT_SECONDS", value: "9223372037"},
		{name: "idle timeout duration", key: "REGISTRY_HUB_UPSTREAM_BODY_IDLE_TIMEOUT_SECONDS", value: "9223372037"},
		{name: "partial TTL duration", key: "REGISTRY_HUB_DOWNLOAD_PARTIAL_TTL_SECONDS", value: "9223372037"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearRegistryHubEnvironment(t)
			t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
			t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
			t.Setenv(test.key, test.value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load accepted overflowing %s", test.key)
			}
		})
	}
}

func clearRegistryHubEnvironment(t *testing.T) {
	t.Helper()
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(name, "REGISTRY_HUB_") || name == "POD_NAMESPACE" {
			t.Setenv(name, "")
		}
	}
}
