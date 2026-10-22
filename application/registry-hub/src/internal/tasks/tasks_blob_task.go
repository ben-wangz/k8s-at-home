package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) EnqueueBlob(ctx context.Context, intent BlobIntent) (Task, bool, error) {
	return m.enqueueBlob(ctx, intent, "", 0)
}

func (m *Manager) EnqueueBlobProtected(ctx context.Context, intent BlobIntent, scopeID string, reservedBytes int64) (Task, bool, error) {
	if intent.ProtectionSessionID == "" || scopeID == "" || reservedBytes < 0 {
		return Task{}, false, errors.New("protected blob task admission is incomplete")
	}
	return m.enqueueBlob(ctx, intent, scopeID, reservedBytes)
}

func (m *Manager) enqueueBlob(ctx context.Context, intent BlobIntent, scopeID string, reservedBytes int64) (Task, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if intent.ID == "" || intent.State != IntentReady && intent.State != IntentRetryWait || intent.Identity.Authority == "" || intent.Identity.Repository == "" || intent.Identity.Digest == "" || intent.MediaType == "" || intent.CacheVersion == "" || intent.ActualSize < 0 || intent.ProtectionSessionID == "" && (scopeID != "" || reservedBytes != 0) || intent.ProtectionSessionID != "" && scopeID == "" {
		return Task{}, false, errors.New("ready blob intent is incomplete")
	}
	if existing, err := m.Get(ctx, intent.ID); err == nil {
		if existing.Kind != BlobKind || existing.Authority != intent.Identity.Authority || existing.Repository != intent.Identity.Repository || existing.BlobDigest != intent.Identity.Digest || existing.BlobSize != intent.ActualSize || existing.CacheVersion != intent.CacheVersion {
			return Task{}, false, errors.New("existing blob task conflicts with its intent")
		}
		return existing, false, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return Task{}, false, err
	}
	active, err := m.listActive(ctx)
	if err != nil {
		return Task{}, false, err
	}
	if runnableTaskCount(active) >= m.pendingMax {
		return Task{}, false, errors.New("solidification queue is full")
	}
	now := time.Now().UTC()
	task := Task{
		SchemaVersion: taskSchemaCurrent, ProtocolVersion: 2, ID: intent.ID, Kind: BlobKind,
		Authority: intent.Identity.Authority, Repository: intent.Identity.Repository,
		BlobDigest: intent.Identity.Digest, BlobSize: intent.ActualSize, CacheVersion: intent.CacheVersion,
		MediaType: intent.MediaType, Discovery: "blob", Reference: intent.Identity.Digest, Generation: intent.Generation, RoundID: intent.ID,
		Status: Pending, Phase: "solidification", CreatedAt: now, UpdatedAt: now,
		LastProgressAt: now, ExpiresAt: now.Add(m.idleTimeout), Checkpoints: make(map[string]string),
	}
	if intent.ProtectionSessionID != "" {
		task.ScopeID = scopeID
		task.ProtectionSessionID = intent.ProtectionSessionID
		task.SessionIDs = []string{intent.ProtectionSessionID}
		task.RequestedBytes = reservedBytes
		task.ReservedBytes = reservedBytes
		task.GCProtected = true
	}
	if err := m.create(ctx, &task); errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) {
		latest, getErr := m.Get(ctx, intent.ID)
		if getErr != nil {
			return Task{}, false, errors.Join(err, getErr)
		}
		return latest, false, nil
	} else if err != nil {
		return Task{}, false, err
	}
	return task, true, nil
}

func (m *Manager) AnchorBlobTask(ctx context.Context, id string) error {
	task, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if task.Kind != BlobKind {
		return errors.New("task is not a blob task")
	}
	if task.Status == Solidified {
		return nil
	}
	if task.Status == Failed || task.Status == Cancelled {
		return storage.ErrConditionFailed
	}
	if task.Status == Running {
		return storage.ErrConditionFailed
	}
	if task.CancelRequested {
		return ErrCancelled
	}
	task.Status = Solidified
	task.WaitReason = ""
	task.NextAttemptAt = time.Time{}
	task.Retryable = false
	task.ErrorClass = ""
	task.ErrorMessage = ""
	task.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &task); err != nil {
		return err
	}
	m.notifyTerminal(ctx, &task)
	return nil
}
