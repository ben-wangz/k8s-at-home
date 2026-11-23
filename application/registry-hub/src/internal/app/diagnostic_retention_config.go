package app

import (
	"strings"
	"time"

	"registry-hub/internal/config"
	"registry-hub/internal/diagnostics"
)

func (s *Service) diagnosticsConfig() config.Diagnostics {
	return s.cfg.Diagnostics.WithDefaults()
}

func (s *Service) diagnosticBatchPolicy() diagnostics.BatchPolicy {
	cfg := s.diagnosticsConfig()
	return diagnostics.BatchPolicy{
		DailyBytes: cfg.EventBatchDailyBytes, DailyBatches: cfg.EventBatchDailyCount,
		GlobalBytes: cfg.EventBatchGlobalBytes, GlobalBatches: cfg.EventBatchGlobalCount,
		DailyCriticalBytes: cfg.EventBatchDailyCriticalBytes, DailyCriticalBatches: cfg.EventBatchDailyCriticalCount,
		GlobalCriticalBytes: cfg.EventBatchGlobalCriticalBytes, GlobalCriticalBatches: cfg.EventBatchGlobalCriticalCount,
	}
}

func (s *Service) sourceSweepDelay(kind string, idleRounds int, retry bool) time.Duration {
	diagnosticsConfig := s.diagnosticsConfig()
	base := diagnosticsConfig.RetentionSweepInterval
	maximum := min(base*4, time.Minute)
	if strings.HasPrefix(kind, "history_") {
		base = diagnosticsConfig.HistorySweepInterval
		maximum = diagnosticsConfig.HistorySweepMaxInterval
	} else if kind != "receipt_cleanup" && kind != "receipt_cleanup_preview" && !strings.HasPrefix(kind, "diagnostic_retention_") {
		return sourceSweepNextDelay(kind, idleRounds, retry)
	}
	if retry || idleRounds < 1 {
		return base
	}
	return idleScanDelay(base, maximum, idleRounds)
}
