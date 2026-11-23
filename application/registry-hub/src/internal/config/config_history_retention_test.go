package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadUsesConfiguredDiagnosticEventFlushInterval(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_DIAGNOSTIC_EVENT_FLUSH_SECONDS", "900")

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured diagnostic event flush interval: %v", err)
	}
	if loaded.Diagnostics.OrdinaryEventFlushInterval != 15*time.Minute {
		t.Fatalf("diagnostic event flush interval = %s, want 15m", loaded.Diagnostics.OrdinaryEventFlushInterval)
	}
}

func TestLoadUsesConfiguredHistoryRetentionDurations(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	for name, value := range map[string]string{
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_FLUSH_SECONDS":            "2",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_CRITICAL_FLUSH_SECONDS":   "1",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_RETENTION_SECONDS":        "90",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BUDGET_RETENTION_SECONDS": "180",
		"REGISTRY_HUB_DIAGNOSTIC_COLLECTION_RETENTION_SECONDS":   "180",
		"REGISTRY_HUB_RETRY_REQUEST_RETENTION_SECONDS":           "180",
		"REGISTRY_HUB_DIAGNOSTIC_TRANSFER_RETENTION_SECONDS":     "180",
		"REGISTRY_HUB_TRANSFER_EXPIRY_RETENTION_SECONDS":         "180",
		"REGISTRY_HUB_TERMINAL_HISTORY_RETENTION_SECONDS":        "20",
		"REGISTRY_HUB_FULL_WORK_RECEIPT_RETENTION_SECONDS":       "30",
		"REGISTRY_HUB_HISTORY_CLEANUP_PREVIEW_TTL_SECONDS":       "20",
		"REGISTRY_HUB_HISTORY_CLEANUP_RESULT_RETENTION_SECONDS":  "20",
		"REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS":            "2",
		"REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS":        "2",
		"REGISTRY_HUB_HISTORY_RETENTION_SWEEP_INTERVAL_SECONDS":  "2",
	} {
		t.Setenv(name, value)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured history retention durations: %v", err)
	}
	got := loaded.Diagnostics
	if got.OrdinaryEventFlushInterval != 2*time.Second || got.CriticalEventFlushInterval != time.Second || got.EventRetention != 90*time.Second || got.EventBudgetRetention != 3*time.Minute ||
		got.CollectedReceiptRetention != 3*time.Minute || got.TransferObservationRetention != 3*time.Minute || got.TransferExpiryRetention != 3*time.Minute ||
		got.RetryRequestRetention != 3*time.Minute ||
		got.TerminalHistoryRetention != 20*time.Second || got.FullWorkReceiptRetention != 30*time.Second || got.HistoryCleanupPreviewTTL != 20*time.Second ||
		got.HistoryCleanupResultRetention != 20*time.Second || got.HistorySweepInterval != 2*time.Second || got.HistorySweepMaxInterval != 2*time.Second || got.RetentionSweepInterval != 2*time.Second {
		t.Fatalf("configured diagnostics durations = %#v", got)
	}
}

func TestLoadRejectsOutOfRangeHistorySweepInterval(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS", "901")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS must be between 1 and 900 seconds") {
		t.Fatalf("Load accepted out-of-range history sweep interval: %v", err)
	}
}

func TestLoadRejectsOutOfRangeHistorySweepMaxInterval(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS", "901")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS must be between 1 and 900 seconds") {
		t.Fatalf("Load accepted out-of-range history sweep maximum: %v", err)
	}
}

func TestLoadRejectsHistorySweepMaximumBelowBaseInterval(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS", "3")
	t.Setenv("REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS", "2")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS must be at least REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS") {
		t.Fatalf("Load accepted history sweep maximum below base interval: %v", err)
	}
}

func TestLoadRejectsIncoherentHistoryRetentionDurations(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_TERMINAL_HISTORY_RETENTION_SECONDS", "30")
	t.Setenv("REGISTRY_HUB_FULL_WORK_RECEIPT_RETENTION_SECONDS", "20")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "full work receipt retention must be at least terminal history retention") {
		t.Fatalf("Load accepted receipt retention shorter than terminal history: %v", err)
	}
}

func TestLoadRequiresHistoryProjectionReadsBeforeReceiptCleanup(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_ENABLED", "true")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED=true") {
		t.Fatalf("load without history projection reads error = %v", err)
	}
	t.Setenv("REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED", "true")
	loaded, err := Load()
	if err != nil {
		t.Fatalf("load migration-ready configuration: %v", err)
	}
	if !loaded.Diagnostics.HistoryProjectionReadsEnabled || !loaded.Diagnostics.FullWorkReceiptCleanupEnabled {
		t.Fatalf("migration configuration = %#v", loaded.Diagnostics)
	}
}

func TestLoadRequiresPreviewBeforeReceiptCleanupAndMakesModesExclusive(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_PREVIEW", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_PREVIEW requires REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED=true") {
		t.Fatalf("load preview without history projection reads error = %v", err)
	}
	t.Setenv("REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED", "true")
	loaded, err := Load()
	if err != nil || !loaded.Diagnostics.FullWorkReceiptCleanupPreview {
		t.Fatalf("load preview configuration = %#v, error = %v", loaded.Diagnostics, err)
	}
	t.Setenv("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_ENABLED", "true")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "cannot both be true") {
		t.Fatalf("load simultaneous preview and cleanup error = %v", err)
	}
}
