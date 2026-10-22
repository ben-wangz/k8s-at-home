package tasks

import (
	"context"
	"errors"
	"reflect"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) Cleanup(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	if olderThan < 24*time.Hour {
		return 0, errors.New("task cleanup retention cannot be shorter than 24 hours")
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	removed := 0
	recordBudget := limit
	token := ""
	for removed < limit && recordBudget > 0 {
		entries, next, err := m.store.List(ctx, "tasks/state/", token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if recordBudget <= 0 {
				return removed, nil
			}
			task, err := m.readTask(ctx, entry.Key)
			if err != nil {
				return removed, err
			}
			if isActive(task.Status) || task.GCProtected || len(task.SessionIDs) > 0 || task.ProtectionSessionID != "" || task.RecoveryReason != "" {
				continue
			}
			if !task.CleanupStarted && task.UpdatedAt.After(cutoff) {
				continue
			}
			records := NewRecords(m.store)
			if task.Kind == BlobKind {
				active, err := m.hasActiveBlobDemand(ctx, records, task.ID)
				if err != nil {
					return removed, err
				}
				if active {
					continue
				}
			}
			if !task.CleanupStarted {
				task.CleanupStarted = true
				if err := m.save(ctx, &task); err != nil {
					if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
						continue
					}
					return removed, err
				}
			}
			batchLimit := min(recordBudget, 500)
			if task.Kind == RootKind {
				n, done, err := records.DeleteTaskGraph(ctx, task.ID, batchLimit)
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
				n, done, err = records.DeleteBlobDemandsForRoot(ctx, task.ID, min(recordBudget, batchLimit))
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
			} else if task.Kind == BlobKind {
				n, done, err := records.DeleteBlobIntentHistory(ctx, task.ID, batchLimit)
				recordBudget -= n
				if err != nil {
					return removed, err
				}
				if !done || recordBudget == 0 {
					continue
				}
			}
			n, done, err := records.DeleteCacheProtectionReferences(ctx, task.ID, min(recordBudget, batchLimit))
			recordBudget -= n
			if err != nil {
				return removed, err
			}
			if !done || recordBudget == 0 {
				continue
			}
			latest, err := m.Get(ctx, task.ID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return removed, err
			}
			if !latest.CleanupStarted || isActive(latest.Status) || latest.GCProtected || len(latest.SessionIDs) > 0 || latest.ProtectionSessionID != "" || latest.RecoveryReason != "" {
				continue
			}
			if err := m.clearWakeRequest(ctx, task.ID); err != nil {
				return removed, err
			}
			if err := m.store.DeleteIfMatch(ctx, entry.Key, latest.version); err != nil {
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			m.finishInFlight(ctx, &task)
			removed++
			recordBudget--
			if removed >= limit {
				return removed, nil
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}

func (m *Manager) hasActiveBlobDemand(ctx context.Context, records *Records, intentID string) (bool, error) {
	token := ""
	for {
		demands, next, err := records.ListBlobDemandRecords(ctx, intentID, token, 500)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		for _, demand := range demands {
			if demand.RootTaskID == "" {
				continue
			}
			root, err := m.Get(ctx, demand.RootTaskID)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			if isActive(root.Status) || root.GCProtected || len(root.SessionIDs) > 0 || root.ProtectionSessionID != "" || root.RecoveryReason != "" {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		token = next
	}
}

func (m *Manager) SaveTerminalProtection(ctx context.Context, task *Task, protected bool, recoveryReason string) error {
	if task == nil {
		return errors.New("task state is missing")
	}
	latest, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if latest.Status != Solidified && latest.Status != Failed && latest.Status != Cancelled && latest.Status != RepairPending {
		return errors.New("task protection state requires a terminal task")
	}
	latest.GCProtected = protected
	latest.RecoveryReason = recoveryReason
	latest.TerminalCleanupComplete = !protected
	latest.QuotaWaitAt = time.Time{}
	latest.QuotaWaitReason = ""
	latest.QuotaWaitBytes = 0
	latest.QuotaReplaceableBytes = 0
	latest.QuotaReplaceableSlots = 0
	latest.ScopeID = task.ScopeID
	latest.SessionIDs = append([]string(nil), task.SessionIDs...)
	latest.RequestedBytes = task.RequestedBytes
	latest.ReservedBytes = task.ReservedBytes
	latest.ProtectionSessionID = task.ProtectionSessionID
	latest.ProtectionOwnerID = task.ProtectionOwnerID
	if !protected {
		latest.ScopeID = ""
		latest.SessionIDs = nil
		latest.RequestedBytes = 0
		latest.ReservedBytes = 0
		latest.ProtectionSessionID = ""
		latest.ProtectionOwnerID = ""
	}
	latest.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &latest); err != nil {
		return err
	}
	*task = latest
	return nil
}

func (m *Manager) Retry(ctx context.Context, id string) (Task, error) {
	task, err := m.Get(ctx, id)
	if err != nil {
		return Task{}, err
	}
	if task.Status != Failed && task.Status != RetryWait {
		return Task{}, errors.New("task is not retryable")
	}
	if task.CleanupStarted {
		return Task{}, errors.New("task history cleanup has started")
	}
	capacityRound := task.Status == Failed && task.ErrorClass == "capacity_limit" && task.Kind == RootKind && task.RootDigest != "" && !task.RootPublished
	if task.Status == Failed && (!task.Retryable && !capacityRound || task.RootDigest == "" && task.BlobDigest == "") {
		return Task{}, errors.New("task is not retryable")
	}
	if capacityRound {
		roundID, err := randomID()
		if err != nil {
			return Task{}, err
		}
		task.RoundID = roundID
		task.Discovery = "manifest"
		task.Graph = make(map[string]GraphEntry)
		task.Checkpoints = make(map[string]string)
		task.GraphVersion++
		task.LastProgressAt = time.Now().UTC()
		task.ExpiresAt = task.LastProgressAt.Add(m.idleTimeout)
		task.QuotaWaitBytes = 0
		task.QuotaReplaceableBytes = 0
		task.QuotaReplaceableSlots = 0
	} else if task.RoundID == "" {
		task.RoundID = task.ID
	}
	clearTaskQuotaWait(&task)
	task.Status = Pending
	task.Phase = "solidification"
	task.WaitReason = ""
	task.NextAttemptAt = time.Time{}
	task.ErrorClass = ""
	task.ErrorMessage = ""
	task.CancelRequested = false
	task.TerminalCleanupComplete = false
	task.Retryable = false
	task.UpdatedAt = time.Now().UTC()
	if err := m.save(ctx, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (m *Manager) Cancel(ctx context.Context, id string) (Task, error) {
	var task Task
	for attempt := 0; attempt < 16; attempt++ {
		current, err := m.Get(ctx, id)
		if err != nil {
			return Task{}, err
		}
		if current.CleanupStarted {
			return Task{}, errors.New("task history cleanup has started")
		}
		switch current.Status {
		case Solidified:
			return current, nil
		case RepairPending:
			return Task{}, errors.New("task cannot be cancelled in its current state")
		case Running:
			if current.CancelRequested {
				return current, nil
			}
			current.CancelRequested = true
			current.CancelPhase = "stop_requested"
		default:
			if current.Status == Cancelled && current.CancelRequested {
				return current, nil
			}
			current.Status = Cancelled
			current.CancelRequested = true
			current.CancelPhase = "quiesced"
		}
		current.UpdatedAt = time.Now().UTC()
		err = m.save(ctx, &current)
		if errors.Is(err, storage.ErrConditionFailed) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			latest, readErr := m.Get(ctx, id)
			if readErr != nil {
				return Task{}, errors.Join(err, readErr)
			}
			if !latest.CancelRequested {
				continue
			}
			current = latest
			err = nil
		}
		if err != nil {
			return Task{}, err
		}
		task = current
		break
	}
	if task.ID == "" {
		return Task{}, storage.ErrUnavailable
	}
	if task.Status == Running {
		if !task.RootPublished {
			m.mu.Lock()
			cancel := m.cancels[id]
			m.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	} else {
		m.finishInFlight(ctx, &task)
		m.notifyTerminal(ctx, &task)
	}
	return task, nil
}

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
