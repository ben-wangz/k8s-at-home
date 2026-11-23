package config

import "time"

type Diagnostics struct {
	OrdinaryEventFlushInterval    time.Duration
	CriticalEventFlushInterval    time.Duration
	EventBatchDailyBytes          int64
	EventBatchDailyCount          int64
	EventBatchGlobalBytes         int64
	EventBatchGlobalCount         int64
	EventBatchDailyCriticalBytes  int64
	EventBatchDailyCriticalCount  int64
	EventBatchGlobalCriticalBytes int64
	EventBatchGlobalCriticalCount int64
	EventRetention                time.Duration
	EventBudgetRetention          time.Duration
	CollectedReceiptRetention     time.Duration
	RetryRequestRetention         time.Duration
	TransferObservationRetention  time.Duration
	TransferExpiryRetention       time.Duration
	TerminalHistoryRetention      time.Duration
	FullWorkReceiptRetention      time.Duration
	HistoryCleanupPreviewTTL      time.Duration
	HistoryCleanupResultRetention time.Duration
	HistorySweepInterval          time.Duration
	HistorySweepMaxInterval       time.Duration
	RetentionSweepInterval        time.Duration
	HistoryProjectionReadsEnabled bool
	FullWorkReceiptCleanupPreview bool
	FullWorkReceiptCleanupEnabled bool
}

func DefaultDiagnostics() Diagnostics {
	return Diagnostics{
		OrdinaryEventFlushInterval:    time.Minute,
		CriticalEventFlushInterval:    5 * time.Second,
		EventBatchDailyBytes:          4 << 20,
		EventBatchDailyCount:          128,
		EventBatchGlobalBytes:         32 << 20,
		EventBatchGlobalCount:         1024,
		EventBatchDailyCriticalBytes:  1 << 20,
		EventBatchDailyCriticalCount:  32,
		EventBatchGlobalCriticalBytes: 8 << 20,
		EventBatchGlobalCriticalCount: 256,
		EventRetention:                7 * 24 * time.Hour,
		EventBudgetRetention:          8 * 24 * time.Hour,
		CollectedReceiptRetention:     30 * 24 * time.Hour,
		RetryRequestRetention:         30 * 24 * time.Hour,
		TransferObservationRetention:  30 * 24 * time.Hour,
		TransferExpiryRetention:       30 * 24 * time.Hour,
		TerminalHistoryRetention:      48 * time.Hour,
		FullWorkReceiptRetention:      3 * 24 * time.Hour,
		HistoryCleanupPreviewTTL:      10 * time.Minute,
		HistoryCleanupResultRetention: 7 * 24 * time.Hour,
		HistorySweepInterval:          15 * time.Second,
		HistorySweepMaxInterval:       5 * time.Minute,
		RetentionSweepInterval:        15 * time.Second,
	}
}

func (c Diagnostics) WithDefaults() Diagnostics {
	defaults := DefaultDiagnostics()
	if c.OrdinaryEventFlushInterval <= 0 {
		c.OrdinaryEventFlushInterval = defaults.OrdinaryEventFlushInterval
	}
	if c.CriticalEventFlushInterval <= 0 {
		c.CriticalEventFlushInterval = defaults.CriticalEventFlushInterval
	}
	if c.EventBatchDailyBytes <= 0 {
		c.EventBatchDailyBytes = defaults.EventBatchDailyBytes
	}
	if c.EventBatchDailyCount <= 0 {
		c.EventBatchDailyCount = defaults.EventBatchDailyCount
	}
	if c.EventBatchGlobalBytes <= 0 {
		c.EventBatchGlobalBytes = defaults.EventBatchGlobalBytes
	}
	if c.EventBatchGlobalCount <= 0 {
		c.EventBatchGlobalCount = defaults.EventBatchGlobalCount
	}
	if c.EventBatchDailyCriticalBytes <= 0 {
		c.EventBatchDailyCriticalBytes = defaults.EventBatchDailyCriticalBytes
	}
	if c.EventBatchDailyCriticalCount <= 0 {
		c.EventBatchDailyCriticalCount = defaults.EventBatchDailyCriticalCount
	}
	if c.EventBatchGlobalCriticalBytes <= 0 {
		c.EventBatchGlobalCriticalBytes = defaults.EventBatchGlobalCriticalBytes
	}
	if c.EventBatchGlobalCriticalCount <= 0 {
		c.EventBatchGlobalCriticalCount = defaults.EventBatchGlobalCriticalCount
	}
	if c.EventRetention <= 0 {
		c.EventRetention = defaults.EventRetention
	}
	if c.CollectedReceiptRetention <= 0 {
		c.CollectedReceiptRetention = defaults.CollectedReceiptRetention
	}
	if c.EventBudgetRetention <= 0 {
		c.EventBudgetRetention = defaults.EventBudgetRetention
	}
	if c.RetryRequestRetention <= 0 {
		c.RetryRequestRetention = defaults.RetryRequestRetention
	}
	if c.TransferObservationRetention <= 0 {
		c.TransferObservationRetention = defaults.TransferObservationRetention
	}
	if c.TransferExpiryRetention <= 0 {
		c.TransferExpiryRetention = defaults.TransferExpiryRetention
	}
	if c.TerminalHistoryRetention <= 0 {
		c.TerminalHistoryRetention = defaults.TerminalHistoryRetention
	}
	if c.FullWorkReceiptRetention <= 0 {
		c.FullWorkReceiptRetention = defaults.FullWorkReceiptRetention
	}
	if c.HistoryCleanupPreviewTTL <= 0 {
		c.HistoryCleanupPreviewTTL = defaults.HistoryCleanupPreviewTTL
	}
	if c.HistoryCleanupResultRetention <= 0 {
		c.HistoryCleanupResultRetention = defaults.HistoryCleanupResultRetention
	}
	if c.HistorySweepInterval <= 0 {
		c.HistorySweepInterval = defaults.HistorySweepInterval
	}
	if c.HistorySweepMaxInterval <= 0 {
		c.HistorySweepMaxInterval = defaults.HistorySweepMaxInterval
	}
	if c.RetentionSweepInterval <= 0 {
		c.RetentionSweepInterval = defaults.RetentionSweepInterval
	}
	return c
}
