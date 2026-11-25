package config

import (
	"os/exec"
	"strconv"
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

func TestLoadParsesHelmHistoryRetentionDurations(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required to verify rendered values")
	}
	keys := []string{"collectionRetentionSeconds", "retryRequestRetentionSeconds", "transferRetentionSeconds", "transferExpiryRetentionSeconds"}
	names := []string{"REGISTRY_HUB_DIAGNOSTIC_COLLECTION_RETENTION_SECONDS", "REGISTRY_HUB_RETRY_REQUEST_RETENTION_SECONDS",
		"REGISTRY_HUB_DIAGNOSTIC_TRANSFER_RETENTION_SECONDS", "REGISTRY_HUB_TRANSFER_EXPIRY_RETENTION_SECONDS"}
	for _, mode := range []string{"defaults", "json"} {
		t.Run(mode, func(t *testing.T) {
			clearRegistryHubEnvironment(t)
			t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
			t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
			args := []string{"template", "retention-test", "../../../chart", "--show-only", "templates/backend-deployment.yaml"}
			want := [4]time.Duration{}
			for i, key := range keys {
				seconds := 2592000
				if mode == "json" {
					seconds += i + 1
					args = append(args, "--set-json", "backend.diagnostics."+key+"="+strconv.Itoa(seconds))
				}
				want[i] = time.Duration(seconds) * time.Second
			}
			rendered, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("render retention configuration: %v: %s", err, rendered)
			}
			for _, name := range names {
				_, tail, found := strings.Cut(string(rendered), "- name: "+name+"\n")
				line, _, _ := strings.Cut(tail, "\n")
				value, err := strconv.Unquote(strings.TrimPrefix(strings.TrimSpace(line), "value: "))
				if !found || err != nil {
					t.Fatalf("rendered %s is missing a quoted value", name)
				}
				t.Setenv(name, value)
			}
			loaded, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			cfg := loaded.Diagnostics
			got := [4]time.Duration{cfg.CollectedReceiptRetention, cfg.RetryRequestRetention, cfg.TransferObservationRetention, cfg.TransferExpiryRetention}
			if got != want {
				t.Fatalf("loaded Helm durations=%v, want %v", got, want)
			}
		})
	}
}

func TestLoadUsesConfiguredDiagnosticEventBatchPolicy(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	for name, value := range map[string]string{
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_BYTES_LIMIT":             "4096",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_COUNT_LIMIT":             "20",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_BYTES_LIMIT":            "32768",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_COUNT_LIMIT":            "100",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_BYTES":  "1024",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_COUNT":  "5",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_BYTES": "8192",
		"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_COUNT": "20",
	} {
		t.Setenv(name, value)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("load configured diagnostic event batch policy: %v", err)
	}
	got := loaded.Diagnostics
	if got.EventBatchDailyBytes != 4096 || got.EventBatchDailyCount != 20 || got.EventBatchGlobalBytes != 32768 ||
		got.EventBatchGlobalCount != 100 || got.EventBatchDailyCriticalBytes != 1024 || got.EventBatchDailyCriticalCount != 5 ||
		got.EventBatchGlobalCriticalBytes != 8192 || got.EventBatchGlobalCriticalCount != 20 {
		t.Fatalf("configured batch policy = %#v", got)
	}
}

func TestLoadRejectsCriticalBatchReserveAboveTotal(t *testing.T) {
	clearRegistryHubEnvironment(t)
	t.Setenv("REGISTRY_HUB_ADMIN_AUTH_CONFIG", `{"basic":[{"username":"admin","password":"secret","permissions":["task-admin"]}]}`)
	t.Setenv("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG", `{"username":"registry","password":"secret"}`)
	t.Setenv("REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_BYTES_LIMIT", "1024")
	t.Setenv("REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_BYTES", "1025")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a critical byte reserve larger than the daily byte limit")
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
