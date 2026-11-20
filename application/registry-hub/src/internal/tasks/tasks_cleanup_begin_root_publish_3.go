package tasks

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) BeginRootPublish(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.CancelRequested && !latest.RootPublishing {
		return ErrCancelled
	}
	if latest.RootPublished {
		*task = latest
		return nil
	}
	if latest.RootPublishing {
		*task = latest
		return nil
	}
	latest.RootPublishing = true
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) MarkRootPublished(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.CancelRequested && !latest.RootPublishing {
		return ErrCancelled
	}
	if latest.RootPublished {
		*task = latest
		return nil
	}
	latest.RootPublishing = false
	latest.RootPublished = true
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) AbortRootPublish(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.RootPublished {
		return ErrRootPublishing
	}
	latest.RootPublishing = false
	latest.UpdatedAt = time.Now().UTC()
	if err := m.SaveProgress(ctx, &latest, currentLease); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) SaveProgress(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	current, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.version != task.version {
		return fmt.Errorf("task progress conflict: task_id=%s stage=version_check expected_version=%q observed_version=%q: %w",
			task.ID, task.version, current.version, storage.ErrConditionFailed)
	}
	task.LegacyProtectionBytes = current.LegacyProtectionBytes
	task.FencingToken = currentLease.FencingToken
	task.UpdatedAt = time.Now().UTC()
	if taskProgressChanged(current, *task) {
		task.LastProgressAt = task.UpdatedAt
		task.ExpiresAt = task.UpdatedAt.Add(m.idleTimeout)
	} else {
		task.LastProgressAt = current.LastProgressAt
		task.ExpiresAt = current.ExpiresAt
	}
	if err := m.save(ctx, task); err != nil {
		if errors.Is(err, storage.ErrConditionFailed) {
			return fmt.Errorf("task progress conflict: task_id=%s stage=conditional_write expected_version=%q: %w",
				task.ID, task.version, err)
		}
		return err
	}
	return nil
}

func (m *Manager) SaveBlobProgressWithProtectionMerge(ctx context.Context, base Task, task *Task, currentLease lease.Lease) error {
	if task == nil || base.ID == "" || task.ID != base.ID || base.Kind != BlobKind || task.Kind != BlobKind {
		return storage.ErrConditionFailed
	}
	target := *task
	for attempt := 0; attempt < 4; attempt++ {
		if err := m.leases.Validate(ctx, currentLease); err != nil {
			return err
		}
		current, err := m.Get(ctx, base.ID)
		if err != nil {
			return err
		}
		if current.version == target.version {
			working := target
			err = m.SaveProgress(ctx, &working, currentLease)
			if err == nil || !errors.Is(err, storage.ErrConditionFailed) {
				if err == nil {
					*task = working
				}
				return err
			}
			continue
		}
		if !compatibleProtectionProgressBase(base, current, target, currentLease) {
			return fmt.Errorf("task protection merge conflict: task_id=%s stage=compatibility: %w", base.ID, storage.ErrConditionFailed)
		}
		baseProtection, currentProtection, targetProtection := taskProtectionGroup(base), taskProtectionGroup(current), taskProtectionGroup(target)
		if !reflect.DeepEqual(currentProtection, baseProtection) && !sameProtectionAssociation(currentProtection, targetProtection) {
			return fmt.Errorf("task protection merge conflict: task_id=%s stage=protection_admission: %w", base.ID, ErrProtectionAdmissionConflict)
		}
		merged := target
		if !reflect.DeepEqual(currentProtection, baseProtection) {
			copyTaskProtection(&merged, current)
		}
		copyTaskReconciliationObservations(&merged, current)
		merged.LegacyProtectionBytes = current.LegacyProtectionBytes
		merged.QuotaWakeRevision, merged.QuotaWakeState = current.QuotaWakeRevision, current.QuotaWakeState
		merged.FencingToken = currentLease.FencingToken
		merged.UpdatedAt = time.Now().UTC()
		if taskProgressChanged(current, merged) {
			merged.LastProgressAt = merged.UpdatedAt
			merged.ExpiresAt = merged.UpdatedAt.Add(m.idleTimeout)
		} else {
			merged.LastProgressAt = current.LastProgressAt
			merged.ExpiresAt = current.ExpiresAt
		}
		merged.version = current.version
		if err := m.leases.Validate(ctx, currentLease); err != nil {
			return err
		}
		if err := m.save(ctx, &merged); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
			continue
		} else if err != nil {
			return err
		}
		*task = merged
		return nil
	}
	return storage.ErrUnavailable
}

func sameProtectionAssociation(left, right taskProtectionState) bool {
	left.ProtectionAssociatedAt, right.ProtectionAssociatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}

type taskProtectionState struct {
	ScopeID, ProtectionSessionID, ProtectionOwnerID, RecoveryReason string
	SessionIDs                                                      []string
	RequestedBytes, ReservedBytes, ProtectedCacheBytes              int64
	GCProtected                                                     bool
	ProtectionAssociatedAt                                          time.Time
}

func taskProtectionGroup(task Task) taskProtectionState {
	return taskProtectionState{
		ScopeID: task.ScopeID, ProtectionSessionID: task.ProtectionSessionID, ProtectionOwnerID: task.ProtectionOwnerID,
		RecoveryReason: task.RecoveryReason, SessionIDs: append([]string(nil), task.SessionIDs...),
		RequestedBytes: task.RequestedBytes, ReservedBytes: task.ReservedBytes, ProtectedCacheBytes: task.ProtectedCacheBytes,
		GCProtected: task.GCProtected, ProtectionAssociatedAt: task.ProtectionAssociatedAt,
	}
}

func copyTaskProtection(target *Task, source Task) {
	target.ScopeID = source.ScopeID
	target.SessionIDs = append([]string(nil), source.SessionIDs...)
	target.RequestedBytes, target.ReservedBytes = source.RequestedBytes, source.ReservedBytes
	target.GCProtected, target.RecoveryReason = source.GCProtected, source.RecoveryReason
	target.ProtectionSessionID, target.ProtectionOwnerID = source.ProtectionSessionID, source.ProtectionOwnerID
	target.ProtectionAssociatedAt, target.ProtectedCacheBytes = source.ProtectionAssociatedAt, source.ProtectedCacheBytes
}

func clearTaskReconciliationObservations(task *Task) {
	task.LastError, task.RecoveredAt = nil, time.Time{}
	task.DiagnosticOutbox = nil
}

func copyTaskReconciliationObservations(target *Task, source Task) {
	target.LastError, target.RecoveredAt = source.LastError, source.RecoveredAt
	target.DiagnosticOutbox = append([]diagnostics.Event(nil), source.DiagnosticOutbox...)
}

func compatibleProtectionProgressBase(base, current, target Task, currentLease lease.Lease) bool {
	if current.ID != base.ID || current.Kind != BlobKind || current.Generation != base.Generation || current.RoundID != base.RoundID ||
		current.Status != Running || current.AttemptID == "" || current.AttemptID != base.AttemptID || current.CancelRequested ||
		current.FencingToken != base.FencingToken || current.FencingToken > currentLease.FencingToken ||
		target.Generation != base.Generation || target.RoundID != base.RoundID || target.AttemptID != base.AttemptID || target.Status != Running || target.CancelRequested {
		return false
	}
	baseState, currentState := base, current
	clearTaskProtection(&baseState)
	clearTaskProtection(&currentState)
	baseState.UpdatedAt, currentState.UpdatedAt = time.Time{}, time.Time{}
	baseState.QuotaWakeRevision, currentState.QuotaWakeRevision = 0, 0
	baseState.QuotaWakeState, currentState.QuotaWakeState = "", ""
	clearTaskReconciliationObservations(&baseState)
	clearTaskReconciliationObservations(&currentState)
	baseState.version, currentState.version = "", ""
	targetContract, baseContract := target, base
	clearTaskProtection(&targetContract)
	clearTaskProtection(&baseContract)
	clearTaskWorkerProgress(&targetContract)
	clearTaskWorkerProgress(&baseContract)
	targetContract.UpdatedAt, baseContract.UpdatedAt = time.Time{}, time.Time{}
	targetContract.LastProgressAt, baseContract.LastProgressAt = time.Time{}, time.Time{}
	targetContract.ExpiresAt, baseContract.ExpiresAt = time.Time{}, time.Time{}
	targetContract.version, baseContract.version = "", ""
	return reflect.DeepEqual(baseState, currentState) && reflect.DeepEqual(baseContract, targetContract)
}

func clearTaskWorkerProgress(task *Task) {
	task.RootDigest, task.BlobDigest = "", ""
	task.WaitReason, task.WaitResourceKey = "", ""
	task.MediaType, task.Discovery = "", ""
	task.Generation, task.GraphVersion = 0, 0
	task.Graph, task.Checkpoints = nil, nil
	task.RootPublishing, task.RootPublished = false, false
	task.CacheVersion, task.BlobSize = "", 0
}

func clearTaskProtection(task *Task) {
	task.ScopeID, task.ProtectionSessionID, task.ProtectionOwnerID, task.RecoveryReason = "", "", "", ""
	task.SessionIDs = nil
	task.RequestedBytes, task.ReservedBytes, task.ProtectedCacheBytes = 0, 0, 0
	task.GCProtected, task.ProtectionAssociatedAt = false, time.Time{}
}

func taskProgressChanged(current, next Task) bool {
	return current.RootDigest != next.RootDigest || current.BlobDigest != next.BlobDigest || current.WaitResourceKey != next.WaitResourceKey ||
		current.MediaType != next.MediaType || current.Generation != next.Generation ||
		current.Discovery != next.Discovery || current.GraphVersion != next.GraphVersion ||
		!reflect.DeepEqual(current.Graph, next.Graph) || !reflect.DeepEqual(current.Checkpoints, next.Checkpoints) ||
		current.RootPublishing != next.RootPublishing || current.RootPublished != next.RootPublished ||
		current.ScopeID != next.ScopeID || current.ProtectionSessionID != next.ProtectionSessionID ||
		current.ProtectionOwnerID != next.ProtectionOwnerID || !reflect.DeepEqual(current.SessionIDs, next.SessionIDs) ||
		current.RequestedBytes != next.RequestedBytes || current.ReservedBytes != next.ReservedBytes ||
		current.LegacyProtectionBytes != next.LegacyProtectionBytes ||
		current.ProtectedCacheBytes != next.ProtectedCacheBytes || current.GCProtected != next.GCProtected ||
		current.CacheVersion != next.CacheVersion || current.BlobSize != next.BlobSize
}

func (m *Manager) CheckActive(ctx context.Context, task *Task, currentLease lease.Lease) error {
	if err := m.leases.Validate(ctx, currentLease); err != nil {
		return err
	}
	current, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.Status == Cancelled || current.CancelRequested {
		return ErrCancelled
	}
	if current.FencingToken > currentLease.FencingToken {
		return lease.ErrLost
	}
	return nil
}

func (m *Manager) Leases() *lease.Manager { return m.leases }
