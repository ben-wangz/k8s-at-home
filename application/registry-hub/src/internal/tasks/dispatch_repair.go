package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

type dispatchRepair struct {
	Cursor inventory.Cursor `json:"cursor"`
	Next   time.Time        `json:"next"`
	Fence  uint64           `json:"fence"`
}

func candidateMatches(task Task, candidate dispatchCandidate) bool {
	return task.Dispatch != nil && task.Dispatch.Signature == dispatchSignature(task) &&
		task.Dispatch.Revision == candidate.Revision && task.ID == candidate.TaskID &&
		task.RoundID == candidate.RoundID && task.Generation == candidate.Generation &&
		task.Dispatch.Candidate != nil && *task.Dispatch.Candidate == candidate
}

func (m *Manager) removeDispatchCandidate(ctx context.Context, key string) error {
	current, err := m.store.Head(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return m.removeObservedDispatchCandidate(ctx, key, current.Version)
}

func (m *Manager) removeObservedDispatchCandidate(ctx context.Context, key, version string) error {
	if version == "" {
		return storage.ErrConditionFailed
	}
	err := m.store.DeleteIfMatch(ctx, key, version)
	if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrConditionFailed) {
		return nil
	}
	return err
}

func (m *Manager) promoteDispatchRetries(ctx context.Context) error {
	entries, _, err := storage.Scan(ctx, m.store, dispatchPrefix+"delayed/", "", 16)
	m.runtimeDispatchReads(0, 0, 1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		candidate, version, err := m.readDispatchCandidate(ctx, entry.Key)
		m.runtimeDispatchReads(1, 0, 0)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			if errors.Is(err, storage.ErrCorruptObject) {
				if err := m.removeObservedDispatchCandidate(ctx, entry.Key, version); err != nil {
					return err
				}
				continue
			}
			return err
		}
		if candidate.Due.IsZero() || entry.Key != dispatchCandidateKey(candidate) {
			if err := m.removeObservedDispatchCandidate(ctx, entry.Key, version); err != nil {
				return err
			}
			continue
		}
		if candidate.Due.After(time.Now()) {
			break
		}
		task, err := m.Get(ctx, candidate.TaskID)
		if err == nil && candidateMatches(task, candidate) && dispatchEligible(task, time.Now()) {
			if err := m.putDispatchCandidateAt(ctx, readyDispatchCandidateKey(candidate), candidate); err != nil && !errors.Is(err, storage.ErrConditionFailed) {
				return err
			}
			if err := m.removeObservedDispatchCandidate(ctx, entry.Key, version); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		} else if err := m.removeObservedDispatchCandidate(ctx, entry.Key, version); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) repairDispatchIndex(ctx context.Context, worker int) error {
	holder, err := m.leases.Acquire(ctx, "dispatch-index-repair", m.owner, time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	defer m.leases.Release(context.Background(), holder)
	const key = dispatchPrefix + "repair.json"
	progress, _, err := inventory.Read[dispatchRepair](ctx, m.store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
		return err
	}
	if time.Now().Before(progress.Next) {
		return nil
	}
	if progress.Cursor.Done {
		progress.Cursor = inventory.Cursor{}
	}
	for count := 0; count < 16; count++ {
		if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < time.Second {
			break
		}
		entry, ok, err := progress.Cursor.Peek(ctx, m.store, "tasks/state/", 16)
		if err != nil || !ok {
			if err != nil {
				return err
			}
			break
		}
		task, err := m.readTask(ctx, entry.Key)
		m.runtimeDispatchReads(0, 1, 0)
		if err == nil {
			err = m.repairDispatchTask(ctx, &task)
			if err == nil && m.dispatchCapacity() && dispatchEligible(task, time.Now()) {
				err = m.dispatchAvailableTask(ctx, worker, task)
			}
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		progress.Cursor.Advance()
		if count == 0 && !m.dispatchCapacity() {
			break
		}
	}
	if progress.Cursor.Done {
		progress.Next = time.Now().Add(30 * time.Second)
	}
	progress.Fence = holder.FencingToken
	if err := m.leases.Validate(ctx, holder); err != nil {
		return err
	}
	err = inventory.Update(ctx, m.store, key, func(current *dispatchRepair) error {
		if current.Fence > holder.FencingToken {
			return storage.ErrConditionFailed
		}
		*current = progress
		return nil
	})
	m.runtimeDispatchCoverage(progress.Cursor.Done, progress.Cursor.Entities)
	return err
}

func (m *Manager) repairDispatchTask(ctx context.Context, task *Task) error {
	if task.Dispatch == nil && !requiresDispatchCandidate(*task) {
		return nil
	}
	if task.Dispatch == nil || task.Dispatch.Signature != dispatchSignature(*task) {
		return m.save(ctx, task)
	}
	if task.Dispatch.Candidate != nil && !task.Dispatch.Pending {
		candidate := *task.Dispatch.Candidate
		observed, _, err := inventory.Read[dispatchCandidate](ctx, m.store, dispatchCandidateKey(candidate))
		if err == nil && observed == candidate {
			return nil
		}
		if !candidate.Due.IsZero() && !candidate.Due.After(time.Now()) && errors.Is(err, storage.ErrNotFound) {
			observed, _, err = inventory.Read[dispatchCandidate](ctx, m.store, readyDispatchCandidateKey(candidate))
			if err == nil && observed == candidate {
				return nil
			}
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
			return err
		}
		return m.restoreDispatchCandidate(ctx, task)
	}
	return m.publishDispatchState(ctx, task)
}

func (m *Manager) restoreDispatchCandidate(ctx context.Context, task *Task) error {
	state := *task.Dispatch
	state.Pending, state.Previous = true, nil
	task.Dispatch = &state
	if err := m.save(ctx, task); err != nil {
		return err
	}
	return nil
}
