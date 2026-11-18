package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

func (m *Manager) dispatchPages(ctx context.Context, worker int) error {
	if err := m.promoteDispatchRetries(ctx); err != nil {
		return err
	}
	for pages := 0; pages < 4; pages++ {
		if !m.dispatchCapacity() {
			return nil
		}
		entries, next, err := storage.Scan(ctx, m.store, dispatchPrefix+"ready/", m.dispatchToken, 32)
		m.runtimeDispatchReads(0, 0, 1)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !m.dispatchCapacity() {
				return nil
			}
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
			task, err := m.Get(ctx, candidate.TaskID)
			if errors.Is(err, storage.ErrNotFound) || err == nil && (!candidateMatches(task, candidate) || entry.Key != readyDispatchCandidateKey(candidate)) {
				if err := m.removeObservedDispatchCandidate(ctx, entry.Key, version); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if dispatchEligible(task, time.Now()) {
				if err := m.dispatchAvailableTask(ctx, worker, task); err != nil {
					return err
				}
			}
		}
		if next == "" {
			m.dispatchToken = ""
			break
		}
		m.dispatchToken = next
	}
	if m.dispatchCapacity() {
		return m.repairDispatchIndex(ctx, worker)
	}
	return nil
}

func (m *Manager) dispatchAvailableTask(ctx context.Context, worker int, task Task) error {
	m.mu.Lock()
	_, active := m.active[task.ID]
	m.mu.Unlock()
	if active {
		return nil
	}
	leased, err := m.leases.Active(ctx, "tasks/"+task.ID)
	if err != nil {
		return err
	}
	if !leased {
		m.dispatchTask(ctx, worker, task)
	}
	return nil
}

func (m *Manager) dispatchCapacity() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.recoveryPaused.Load() && !m.admissionBlocked.Load() && len(m.active) < m.workers
}

// WakeDispatch resumes task dispatch after processor capacity becomes available.
func (m *Manager) WakeDispatch() {
	m.admissionBlocked.Store(false)
	select {
	case m.dispatchWake <- struct{}{}:
	default:
	}
}

func (m *Manager) dispatchTask(ctx context.Context, worker int, task Task) {
	m.mu.Lock()
	_, running := m.active[task.ID]
	if m.recoveryPaused.Load() || running || len(m.active) >= m.workers {
		m.mu.Unlock()
		return
	}
	m.active[task.ID] = struct{}{}
	m.mu.Unlock()
	m.runtimeTask(task.ID, true)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			m.mu.Lock()
			delete(m.active, task.ID)
			m.mu.Unlock()
			m.runtimeTask(task.ID, false)
			if !m.admissionBlocked.Load() {
				m.WakeDispatch()
			}
		}()
		m.process(ctx, worker, task)
	}()
}
