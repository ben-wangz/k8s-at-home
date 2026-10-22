package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

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
