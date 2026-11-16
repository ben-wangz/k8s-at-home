package app

import (
	"context"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

const backgroundStatusPublishInterval = 30 * time.Second

type sourceSweepCheckpoint struct {
	QuotaLastCandidates map[string]tasks.Task    `json:"quotaLastCandidates,omitempty"`
	QuotaSignalRevision uint64                   `json:"quotaSignalRevision,omitempty"`
	QuotaGateState      string                   `json:"quotaGateState,omitempty"`
	QuotaCandidates     map[string]tasks.Task    `json:"quotaCandidates,omitempty"`
	QuotaWakeIndex      int                      `json:"quotaWakeIndex,omitempty"`
	Status              backgroundScanStatus     `json:"status"`
	Cursor              inventory.Cursor         `json:"cursor"`
	Backoff             map[string]entityBackoff `json:"backoff"`
	Round               uint64                   `json:"round"`
	IdleRounds          int                      `json:"idleRounds,omitempty"`
	RoundFailed         bool                     `json:"roundFailed"`
	ScanRetries         int                      `json:"scanRetries"`
	NextRound           time.Time                `json:"nextRound"`
	DiagnosticOutbox    []diagnostics.Event      `json:"diagnosticOutbox,omitempty"`
	StatusPublishedAt   time.Time                `json:"statusPublishedAt,omitempty"`
}

func sourceSweepKey(kind string) string { return "background/v1/source-checkpoints/" + kind + ".json" }

func (s *Service) saveSourceSweep(ctx context.Context, holder lease.Lease, kind string, checkpoint sourceSweepCheckpoint) error {
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error { return lease.NewManager(s.store).Validate(ctx, holder) })
	var publishStatus bool
	if err := inventory.Update(ctx, s.store, sourceSweepKey(kind), func(value *sourceSweepCheckpoint) error {
		if value.Status.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		publishedAt := value.StatusPublishedAt
		publishStatus = shouldPublishBackgroundStatus(value.Status, checkpoint.Status, publishedAt, time.Now().UTC())
		if publishStatus {
			checkpoint.StatusPublishedAt = time.Now().UTC()
		} else {
			checkpoint.StatusPublishedAt = publishedAt
		}
		*value = checkpoint
		return nil
	}); err != nil {
		return err
	}
	if !publishStatus {
		return nil
	}
	return inventory.Update(ctx, s.store, "background/v1/scans/"+kind+".json", func(value *backgroundScanStatus) error {
		if value.Fence > holder.FencingToken {
			return lease.ErrLost
		}
		*value = checkpoint.Status
		return nil
	})
}

func shouldPublishBackgroundStatus(previous, next backgroundScanStatus, publishedAt, now time.Time) bool {
	return publishedAt.IsZero() || now.Sub(publishedAt) >= backgroundStatusPublishInterval ||
		previous.Owner != next.Owner || previous.Fence != next.Fence || !previous.Complete && next.Complete ||
		!previous.ErrorAt.Equal(next.ErrorAt) || previous.ErrorClass != next.ErrorClass ||
		previous.ErrorRecovered != next.ErrorRecovered || previous.WaitReason != next.WaitReason ||
		previous.DependencyGeneration != next.DependencyGeneration
}

func readCheckpointStatus[T any](ctx context.Context, store storage.Store, checkpointKey, legacyKey string, status func(T) backgroundScanStatus) (backgroundScanStatus, error) {
	value, _, err := inventory.Read[T](ctx, store, checkpointKey)
	if err == nil {
		return status(value), nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return backgroundScanStatus{}, err
	}
	legacy, _, err := inventory.Read[backgroundScanStatus](ctx, store, legacyKey)
	return legacy, err
}

func readSourceScanStatus(ctx context.Context, store storage.Store, kind string) (backgroundScanStatus, error) {
	return readCheckpointStatus(ctx, store, sourceSweepKey(kind), "background/v1/scans/"+kind+".json", func(value sourceSweepCheckpoint) backgroundScanStatus { return value.Status })
}
