package app

import (
	"registry-hub/internal/config"
	"testing"
	"time"
)

func TestDiagnosticRetentionSweepUsesConfiguredShortInterval(t *testing.T) {
	service := &Service{cfg: config.Config{Diagnostics: config.Diagnostics{RetentionSweepInterval: 2 * time.Second}}}
	if got := service.sourceSweepDelay("diagnostic_retention_events", 1, false); got != 2*time.Second {
		t.Fatalf("first retention scan delay = %s, want 2s", got)
	}
	if got := service.sourceSweepDelay("diagnostic_retention_events", 2, false); got != 4*time.Second {
		t.Fatalf("idle retention scan delay = %s, want 4s", got)
	}
	if got := service.sourceSweepDelay("diagnostic_retention_events", 3, true); got != 2*time.Second {
		t.Fatalf("retry retention scan delay = %s, want 2s", got)
	}
	if got := service.sourceSweepDelay("history_receipts", 1, false); got != 15*time.Second {
		t.Fatalf("unrelated source scan delay = %s, want default 15s", got)
	}
	for _, kind := range []string{"receipt_cleanup", "receipt_cleanup_preview"} {
		if got := service.sourceSweepDelay(kind, 1, false); got != 2*time.Second {
			t.Fatalf("%s first scan delay = %s, want configured 2s", kind, got)
		}
		if got := service.sourceSweepDelay(kind, 3, false); got != 8*time.Second {
			t.Fatalf("%s idle scan cap = %s, want derived 8s", kind, got)
		}
		if got := service.sourceSweepDelay(kind, 3, true); got != 2*time.Second {
			t.Fatalf("%s retry scan delay = %s, want configured 2s", kind, got)
		}
	}
}

func TestHistorySweepUsesConfiguredIntervalAndMaximum(t *testing.T) {
	service := &Service{cfg: config.Config{Diagnostics: config.Diagnostics{
		HistorySweepInterval:    2 * time.Second,
		HistorySweepMaxInterval: 8 * time.Second,
	}}}
	if got := service.sourceSweepDelay("history_receipts", 1, false); got != 2*time.Second {
		t.Fatalf("first history scan delay = %s, want 2s", got)
	}
	if got := service.sourceSweepDelay("history_work", 2, false); got != 4*time.Second {
		t.Fatalf("idle history scan delay = %s, want 4s", got)
	}
	if got := service.sourceSweepDelay("history_work", 10, false); got != 8*time.Second {
		t.Fatalf("history scan backoff cap = %s, want configured 8s", got)
	}
	if got := service.sourceSweepDelay("history_receipts", 3, true); got != 2*time.Second {
		t.Fatalf("retry history scan delay = %s, want 2s", got)
	}

	defaultMaximum := &Service{cfg: config.Config{Diagnostics: config.Diagnostics{HistorySweepInterval: 2 * time.Second}}}
	if got := defaultMaximum.sourceSweepDelay("history_work", 10, false); got != 5*time.Minute {
		t.Fatalf("default history scan backoff cap = %s, want 5m", got)
	}
}
