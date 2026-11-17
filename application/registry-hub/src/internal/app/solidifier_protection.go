package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"registry-hub/internal/config"
	"registry-hub/internal/tasks"
	"time"
)

func protectionLimits(cfg config.Config) (int, int64) {
	maxSessions := cfg.Solidification.MaxProtectionSessions
	if maxSessions < 1 {
		maxSessions = 1024
	}
	maxBytes := cfg.Solidification.MaxUnanchoredDistributionBytes
	if maxBytes < 1 {
		maxBytes = cfg.Limits.ClosureBytes
	}
	if maxBytes < 1 {
		maxBytes = 1 << 40
	}
	return maxSessions, maxBytes
}

func markQuotaWaitPosition(task *tasks.Task, reason string) time.Time {
	if task.QuotaWaitAt.IsZero() || task.QuotaWaitReason != reason {
		task.QuotaWaitAt = time.Now().UTC()
		task.QuotaWaitReason = reason
	}
	return task.QuotaWaitAt
}

func protectedCacheLimit(cfg config.Config) int64 {
	limit := cfg.Solidification.MaxProtectedCacheBytes
	if limit < 1 {
		limit = cfg.Limits.DownloadMaxBytes
	}
	if limit < 1 {
		limit = 200 << 30
	}
	return limit
}

func protectionScopeID(authority, repository, digest string, generation uint64) (string, error) {
	identity := struct {
		Authority  string `json:"authority"`
		Repository string `json:"repository"`
		Digest     string `json:"digest"`
		Generation uint64 `json:"generation"`
	}{authority, repository, digest, generation}
	data, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func hashID(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

func taskProtectionSession(task *tasks.Task) string {
	if task.ProtectionSessionID != "" {
		return task.ProtectionSessionID
	}
	return task.ID
}

func isDigestReference(reference string) bool {
	return artifactDigest(reference) == nil
}

func (s *solidifier) beginProtectedOperation(ctx context.Context, sessionID string) (func() error, error) {
	return s.beginProtectedOperationMode(ctx, sessionID, false)
}

func (s *solidifier) beginProtectedVerificationOperation(ctx context.Context, sessionID string) (func() error, error) {
	return s.beginProtectedOperationMode(ctx, sessionID, true)
}

func (s *solidifier) beginProtectedOperationMode(ctx context.Context, sessionID string, verification bool) (func() error, error) {
	if sessionID == "" {
		release, err := s.gate.Enter(ctx)
		if err != nil {
			return nil, temporary("gc_admission", err)
		}
		return func() error {
			release()
			return nil
		}, nil
	}
	var finish func(context.Context) error
	var err error
	if verification {
		finish, err = s.gate.BeginProtectionVerificationOperation(ctx, sessionID)
	} else {
		finish, err = s.gate.BeginProtectionOperation(ctx, sessionID)
	}
	if err != nil {
		if errors.Is(err, ErrProtectionReconcilePending) {
			return nil, tasks.WaitForResource(tasks.WaitingDeps, "protection_reconcile_pending", "protection/"+sessionID)
		} else if errors.Is(err, ErrProtectionBusy) {
			return nil, tasks.WaitForResource(tasks.WaitingDeps, "protection_busy", "protection/"+sessionID)
		}
		return nil, temporary("protection_operation", err)
	}
	return func() error {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := finish(finishCtx)
		s.service.signalTaskReconciliation()
		return err
	}, nil
}

func (s *solidifier) finishProtectedOperation(finish func() error, result *error) {
	if err := finish(); err != nil {
		*result = errors.Join(*result, temporary("protection_operation", err))
	}
}

func (s *solidifier) markRemoteWrite(ctx context.Context, sessionID, permitID string, remoteWrite protectionRemoteWrite) error {
	if sessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteStartedWithPermit(ctx, sessionID, permitID, remoteWrite); err != nil {
		if errors.Is(err, ErrProtectionReconcilePending) {
			return tasks.WaitForResource(tasks.WaitingDeps, "protection_reconcile_pending", "protection/"+sessionID)
		} else if errors.Is(err, ErrProtectionBusy) {
			return tasks.WaitForResource(tasks.WaitingDeps, "protection_busy", "protection/"+sessionID)
		}
		return temporary("protection_write_state", err)
	}
	return nil
}

func (s *Service) acquireProtectionWritePermit(ctx context.Context, sessionID string) (string, func() error, error) {
	if sessionID == "" {
		return "", func() error { return nil }, nil
	}
	permitID := hashID("protection-write-permit", requestID())
	if err := s.gate.AcquireProtectionWritePermit(ctx, sessionID, permitID); errors.Is(err, ErrProtectionReconcilePending) {
		return "", nil, tasks.WaitForResource(tasks.WaitingDeps, "protection_reconcile_pending", "protection/"+sessionID)
	} else if errors.Is(err, ErrProtectionBusy) {
		return "", nil, tasks.WaitForResource(tasks.WaitingDeps, "protection_busy", "protection/"+sessionID)
	} else if err != nil {
		return "", nil, temporary("protection_write_permit", err)
	}
	return permitID, func() error {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.gate.ReleaseProtectionWritePermit(releaseCtx, sessionID, permitID); err != nil {
			return err
		}
		s.signalTaskReconciliation()
		return nil
	}, nil
}

func waitForObjectLease(authority, repository, digest string) error {
	return tasks.WaitForResource(tasks.WaitingDeps, "object_lease_pending", "objects/"+objectLeaseKey(authority, repository, digest))
}

func (s *solidifier) markRemoteContentObserved(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	return s.gate.MarkProtectionContentObserved(ctx, sessionID)
}

func (s *solidifier) resolveRemoteWrite(ctx context.Context, sessionID string, remoteWrite protectionRemoteWrite) error {
	if sessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteResolved(ctx, sessionID, remoteWrite); err != nil {
		return temporary("protection_write_state", err)
	}
	s.service.signalTaskReconciliation()
	return nil
}
