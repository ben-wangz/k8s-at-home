package app

import (
	"context"
	"time"
)

func (s *Service) startBackgroundWorkers(ctx context.Context) {
	s.backgroundSlots = make(chan struct{}, 2)
	s.backgroundScheduler = &backgroundScheduler{capacity: cap(s.backgroundSlots)}
	s.backgroundReady = map[string]chan struct{}{}
	s.backgroundWake = newBackgroundScanWakeChannels()
	s.backgroundRetryWake = newBackgroundRetryWakeChannels()
	s.collectorSlots = make(chan struct{}, 1)
	collectorPace := time.NewTicker(250 * time.Millisecond)
	s.collectorPace = collectorPace.C
	pace := time.NewTicker(100 * time.Millisecond)
	s.backgroundPace = pace.C
	s.cache.SetProjectionRetention(s.retainUnprojectedManifest)
	workers := []func(context.Context){s.runInventory, s.watchReadiness, s.refreshMetrics, s.runTaskMetrics, s.runCacheMetrics, s.runIntentMetrics, s.runProtectionMetrics, s.runReferrerRepairs, s.runCacheCleanup, s.runBlobIntentReconciliation, s.runBackgroundReconciliation, s.runHealthProbes}
	workers = append(workers, s.runDiagnosticHistory)
	workers = append(workers, s.runStorageInventory)
	workers = append(workers, s.runDiagnosticEventSink)
	workers = append(workers, s.runDiagnosticSourceWakeSink)
	workers = append(workers, s.runDiagnosticRetention)
	workers = append(workers, s.runRetryRequestRetention)
	workers = append(workers, func(ctx context.Context) { <-ctx.Done(); pace.Stop(); collectorPace.Stop() })
	queues := []struct {
		kind string
		step backgroundStep
	}{{"inventory", s.projectManifestStep}, {"execution", s.projectExecutionStep}, {"download_cleanup", s.cleanupDownloadStep}, {"notify", s.notificationStep}, {"wake_root", s.wakeRootStep}, {"terminal_cleanup", s.terminalCleanupStep}, {"quota_wake", s.quotaWakeStep}, {"cache_verification", s.cacheVerificationStep}, {"distribution_verification", s.distributionVerificationStep}, {historyCleanupWorkKind, s.historyCleanupStep}, {historyCleanupPreviewKind, s.historyCleanupRangePreviewStep}}
	for _, queue := range queues {
		s.backgroundReady[queue.kind] = make(chan struct{}, 1)
		workers = append(workers, func(ctx context.Context) { s.runBackgroundQueue(ctx, queue.kind, queue.step) })
	}
	for _, worker := range workers {
		s.background.Add(1)
		go func() { defer s.background.Done(); worker(ctx) }()
	}
}
