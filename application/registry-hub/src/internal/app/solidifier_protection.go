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

func (s *solidifier) beginProtectedOperation(ctx context.Context, task *tasks.Task) (func() error, error) {
	sessionID := taskProtectionSession(task)
	if task.ProtectionSessionID == "" {
		release, err := s.gate.Enter(ctx)
		if err != nil {
			return nil, temporary("gc_admission", err)
		}
		return func() error {
			release()
			return nil
		}, nil
	}
	finish, err := s.gate.BeginProtectionOperation(ctx, sessionID)
	if err != nil {
		return nil, temporary("protection_operation", err)
	}
	return func() error {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return finish(finishCtx)
	}, nil
}

func (s *solidifier) finishProtectedOperation(finish func() error, result *error) {
	if err := finish(); err != nil {
		*result = errors.Join(*result, temporary("protection_operation", err))
	}
}

func (s *solidifier) markRemoteWrite(ctx context.Context, task *tasks.Task, remoteWrite protectionRemoteWrite) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteStarted(ctx, taskProtectionSession(task), remoteWrite); err != nil {
		return temporary("protection_write_state", err)
	}
	return nil
}

func (s *solidifier) markRemoteContentObserved(ctx context.Context, task *tasks.Task) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	return s.gate.MarkProtectionContentObserved(ctx, task.ProtectionSessionID)
}

func (s *solidifier) resolveRemoteWrite(ctx context.Context, task *tasks.Task, remoteWrite protectionRemoteWrite) error {
	if task.ProtectionSessionID == "" {
		return nil
	}
	if err := s.gate.MarkProtectionWriteResolved(ctx, taskProtectionSession(task), remoteWrite); err != nil {
		return temporary("protection_write_state", err)
	}
	return nil
}
