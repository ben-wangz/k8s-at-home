package tasks

import (
	"context"
	"reflect"
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
		return storage.ErrConditionFailed
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
	return m.save(ctx, task)
}

func taskProgressChanged(current, next Task) bool {
	return current.RootDigest != next.RootDigest || current.BlobDigest != next.BlobDigest ||
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
