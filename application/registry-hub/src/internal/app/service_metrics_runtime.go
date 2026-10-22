package app

import (
	"context"
	"registry-hub/internal/tasks"
	"strings"
	"time"
)

func (s *Service) refreshCacheMetrics(ctx context.Context) {
	stats, err := s.cache.Stats(ctx, time.Hour)
	if err == nil {
		s.metrics.setCacheStats(stats)
	}
}

func (s *Service) refreshDownloadAndReplayMetrics(ctx context.Context) {
	if reserved, sessions, err := s.downloadManager().Usage(ctx); err == nil {
		s.metrics.partialReserved.Set(float64(reserved))
		s.metrics.partialSessions.Set(float64(sessions))
		s.metrics.partialLimit.Set(float64(s.cfg.Limits.DownloadMaxBytes))
	}
	if provider, ok := s.store.(interface{ ReplayUsage() (int64, int64, int) }); ok {
		memory, disk, active := provider.ReplayUsage()
		s.metrics.s3ReplayMemory.Set(float64(memory))
		s.metrics.s3ReplayDisk.Set(float64(disk))
		s.metrics.s3ReplayActive.Set(float64(active))
	} else {
		s.metrics.s3ReplayMemory.Set(0)
		s.metrics.s3ReplayDisk.Set(0)
		s.metrics.s3ReplayActive.Set(0)
	}
}

func taskAuthorityClass(authority string) string {
	if authority == "" {
		return "local"
	}
	if strings.EqualFold(authority, "docker.io") {
		return "docker"
	}
	return "public"
}

func stableTaskErrorClass(class string) string {
	switch class {
	case "upstream", "manifest_invalid", "storage", "referrer_index", "lease":
		return class
	default:
		return "other"
	}
}

func stableTaskKind(kind tasks.Kind) string {
	switch kind {
	case tasks.RootKind:
		return "root"
	case tasks.BlobKind:
		return "blob"
	default:
		return "other"
	}
}

func stableBlobIntentState(state tasks.BlobIntentState) string {
	for _, candidate := range blobIntentMetricStates[:len(blobIntentMetricStates)-1] {
		if string(state) == candidate {
			return candidate
		}
	}
	return "other"
}

func stableTaskWaitReason(reason string) string {
	for _, candidate := range taskWaitReasonLabels[:len(taskWaitReasonLabels)-1] {
		if reason == candidate {
			return reason
		}
	}
	return "other"
}

func stableTaskCancelPhase(phase string) string {
	switch phase {
	case "stop_requested", "quiesced":
		return phase
	default:
		return "other"
	}
}
