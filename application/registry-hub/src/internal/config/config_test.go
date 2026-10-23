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
	if loaded.Limits.ObjectBytes != 100<<30 || loaded.Limits.ClosureObjects != 10000 || loaded.Limits.ClosureBytes != 1<<40 || loaded.Limits.ClosureDepth != 64 || loaded.Limits.RequestTimeout != 300*time.Second || loaded.Limits.BlobTransferTimeout != 24*time.Hour || loaded.Limits.UpstreamBodyIdleTimeout != 60*time.Second || loaded.Limits.DownloadFinalizeTimeout != 300*time.Second || loaded.Limits.DownloadPartialTTL != 24*time.Hour || loaded.Limits.DownloadMaxBytes != 200<<30 {
		t.Fatalf("default limits = object_bytes:%d objects:%d bytes:%d depth:%d request_timeout:%s blob_timeout:%s idle_timeout:%s finalize_timeout:%s partial_ttl:%s download_bytes:%d", loaded.Limits.ObjectBytes, loaded.Limits.ClosureObjects, loaded.Limits.ClosureBytes, loaded.Limits.ClosureDepth, loaded.Limits.RequestTimeout, loaded.Limits.BlobTransferTimeout, loaded.Limits.UpstreamBodyIdleTimeout, loaded.Limits.DownloadFinalizeTimeout, loaded.Limits.DownloadPartialTTL, loaded.Limits.DownloadMaxBytes)
	}
	if loaded.S3.Timeout != 300*time.Second || loaded.S3.ReplayMemoryBytes != 16<<20 || loaded.S3.ReplayMaxMemoryBytes != 64<<20 || loaded.S3.ReplayMaxDiskBytes != loaded.Limits.DownloadMaxBytes || loaded.S3.ReplayDirectory != "/tmp/registry-hub-s3-replay" {
		t.Fatalf("default S3 replay config = timeout:%s memory:%d max_memory:%d max_disk:%d directory:%q", loaded.S3.Timeout, loaded.S3.ReplayMemoryBytes, loaded.S3.ReplayMaxMemoryBytes, loaded.S3.ReplayMaxDiskBytes, loaded.S3.ReplayDirectory)
	}
	if loaded.Solidification.IdleTimeout != 24*time.Hour || loaded.Solidification.MaxWaitingRoots != 1024 || loaded.Solidification.MaxBlobIntents != 10000 || loaded.Solidification.MaxProtectionSessions != 1024 || loaded.Solidification.MaxProtectedCacheBytes != loaded.Limits.DownloadMaxBytes || loaded.Solidification.MaxUnanchoredDistributionBytes != loaded.Limits.ClosureBytes {
		t.Fatalf("default solidification config = %#v", loaded.Solidification)
	}
}

func TestLoadUsesConfiguredSolidificationLimits(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_IDLE_TIMEOUT_SECONDS", "3600")
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_MAX_WAITING_ROOTS", "32")
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_MAX_BLOB_INTENTS", "512")
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTION_SESSIONS", "64")
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTED_CACHE_BYTES", "8192")
	t.Setenv("REGISTRY_HUB_SOLIDIFICATION_MAX_UNANCHORED_DISTRIBUTION_BYTES", "16384")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured solidification limits: %v", err)
	}
	if loaded.Solidification.IdleTimeout != time.Hour || loaded.Solidification.MaxWaitingRoots != 32 || loaded.Solidification.MaxBlobIntents != 512 || loaded.Solidification.MaxProtectionSessions != 64 || loaded.Solidification.MaxProtectedCacheBytes != 8192 || loaded.Solidification.MaxUnanchoredDistributionBytes != 16384 {
		t.Fatalf("configured solidification limits = %#v", loaded.Solidification)
	}
}

func TestLoadUsesConfiguredDownloadFinalizeTimeout(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_DOWNLOAD_FINALIZE_TIMEOUT_SECONDS", "45")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured download finalization timeout: %v", err)
	}
	if loaded.Limits.DownloadFinalizeTimeout != 45*time.Second {
		t.Fatalf("download finalization timeout = %s, want 45s", loaded.Limits.DownloadFinalizeTimeout)
	}
}

func TestLoadUsesConfiguredS3ReplayLimits(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_S3_REPLAY_MEMORY_BYTES", "1024")
	t.Setenv("REGISTRY_HUB_S3_REPLAY_MAX_MEMORY_BYTES", "4096")
	t.Setenv("REGISTRY_HUB_S3_REPLAY_MAX_DISK_BYTES", "8192")
	t.Setenv("REGISTRY_HUB_S3_REPLAY_DIRECTORY", "/var/tmp/replay")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured S3 replay limits: %v", err)
	}
	if loaded.S3.ReplayMemoryBytes != 1024 || loaded.S3.ReplayMaxMemoryBytes != 4096 || loaded.S3.ReplayMaxDiskBytes != 8192 || loaded.S3.ReplayDirectory != "/var/tmp/replay" {
		t.Fatalf("configured S3 replay config = memory:%d max_memory:%d max_disk:%d directory:%q", loaded.S3.ReplayMemoryBytes, loaded.S3.ReplayMaxMemoryBytes, loaded.S3.ReplayMaxDiskBytes, loaded.S3.ReplayDirectory)
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
		{name: "download finalization duration", key: "REGISTRY_HUB_DOWNLOAD_FINALIZE_TIMEOUT_SECONDS", value: "9223372037"},
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
