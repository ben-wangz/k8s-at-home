package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"time"
)

const dispatchPrefix = "tasks/dispatch/v1/"

type dispatchCandidate struct {
	TaskID     string    `json:"taskID"`
	RoundID    string    `json:"roundID"`
	Generation uint64    `json:"generation"`
	Revision   string    `json:"revision"`
	Due        time.Time `json:"due,omitempty"`
	CreatedAt  time.Time `json:"createdAt"`
}

type DispatchState struct {
	Signature string             `json:"signature"`
	Revision  string             `json:"revision"`
	Pending   bool               `json:"pending"`
	Candidate *dispatchCandidate `json:"candidate,omitempty"`
	Previous  *dispatchCandidate `json:"previous,omitempty"`
}

func (m *Manager) readDispatchCandidate(ctx context.Context, key string) (dispatchCandidate, string, error) {
	var candidate dispatchCandidate
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return candidate, "", err
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	err = errors.Join(err, object.Body.Close())
	if err != nil {
		return candidate, object.Version, err
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &candidate) != nil {
		return candidate, object.Version, storage.ErrCorruptObject
	}
	return candidate, object.Version, nil
}

func dispatchSignature(task Task) string {
	return inventory.ID(task.ID, task.RoundID, fmt.Sprint(task.Generation), string(task.Status), task.Phase,
		task.NextAttemptAt.UTC().Format(time.RFC3339Nano), task.AttemptID, fmt.Sprint(task.CancelRequested))
}

func prepareDispatchState(task *Task) {
	signature := dispatchSignature(*task)
	if task.Dispatch != nil && task.Dispatch.Signature == signature {
		return
	}
	next := &DispatchState{Signature: signature, Pending: true}
	if task.Dispatch != nil {
		next.Previous = task.Dispatch.Candidate
		next.Revision = task.Dispatch.Revision
	}
	next.Revision = inventory.ID(next.Revision, signature)
	if requiresDispatchCandidate(*task) {
		next.Candidate = &dispatchCandidate{TaskID: task.ID, RoundID: task.RoundID, Generation: task.Generation,
			Revision: next.Revision, CreatedAt: task.CreatedAt}
		if task.Status == RetryWait {
			next.Candidate.Due = task.NextAttemptAt
		}
	}
	task.Dispatch = next
}

func requiresDispatchCandidate(task Task) bool {
	return task.Phase == "solidification" && (task.Status == Pending || task.Status == Running || task.Status == RetryWait)
}

func dispatchCandidateKey(candidate dispatchCandidate) string {
	prefix, order := "ready/", candidate.CreatedAt
	if !candidate.Due.IsZero() {
		prefix, order = "delayed/", candidate.Due
	}
	stamp := max(int64(0), order.UnixNano())
	return fmt.Sprintf("%s%s%020d-%s-%s.json", dispatchPrefix, prefix, stamp, candidate.TaskID, candidate.Revision)
}

func readyDispatchCandidateKey(candidate dispatchCandidate) string {
	ready := candidate
	ready.Due = time.Time{}
	return dispatchCandidateKey(ready)
}

func (m *Manager) publishDispatchState(ctx context.Context, task *Task) error {
	state := task.Dispatch
	if state == nil || !state.Pending {
		return nil
	}
	holder, err := m.leases.Acquire(ctx, "dispatch-index/"+task.ID, m.owner, time.Minute)
	if err != nil {
		return err
	}
	defer m.leases.Release(context.Background(), holder)
	current, err := m.Get(ctx, task.ID)
	if err != nil {
		return err
	}
	if current.Dispatch == nil || !current.Dispatch.Pending {
		*task = current
		return nil
	}
	*task, state = current, current.Dispatch
	ctx = inventory.WithWriteGuard(ctx, func(check context.Context) error {
		return m.leases.Validate(check, holder)
	})
	return m.publishDispatchStateHeld(ctx, task)
}

func (m *Manager) publishDispatchStateHeld(ctx context.Context, task *Task) error {
	state := task.Dispatch
	if state.Candidate != nil {
		if err := m.putDispatchCandidate(ctx, *state.Candidate); err != nil {
			return err
		}
	}
	if state.Previous != nil {
		keys := []string{dispatchCandidateKey(*state.Previous)}
		if !state.Previous.Due.IsZero() {
			keys = append(keys, readyDispatchCandidateKey(*state.Previous))
		}
		for _, key := range keys {
			if err := inventory.ValidateWriteGuard(ctx); err != nil {
				return err
			}
			if err := m.removeDispatchCandidate(ctx, key); err != nil {
				return err
			}
		}
	}
	return m.ackDispatchState(ctx, task)
}

func (m *Manager) putDispatchCandidate(ctx context.Context, candidate dispatchCandidate) error {
	return m.putDispatchCandidateAt(ctx, dispatchCandidateKey(candidate), candidate)
}

func (m *Manager) putDispatchCandidateAt(ctx context.Context, key string, candidate dispatchCandidate) error {
	data, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 4; attempt++ {
		current, _, readErr := m.readDispatchCandidate(ctx, key)
		if errors.Is(readErr, storage.ErrNotFound) {
			if err := inventory.ValidateWriteGuard(ctx); err != nil {
				return err
			}
			_, err = m.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
		} else if readErr != nil && !errors.Is(readErr, storage.ErrCorruptObject) {
			return readErr
		} else if readErr == nil && current == candidate {
			return nil
		} else {
			entry, headErr := m.store.Head(ctx, key)
			if errors.Is(headErr, storage.ErrNotFound) {
				continue
			}
			if headErr != nil {
				return headErr
			}
			if err := inventory.ValidateWriteGuard(ctx); err != nil {
				return err
			}
			_, err = m.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: entry.Version})
		}
		if err == nil {
			return nil
		}
		if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
			return err
		}
		observed, _, confirmErr := m.readDispatchCandidate(ctx, key)
		if confirmErr == nil && observed == candidate {
			return nil
		}
		if confirmErr != nil && !errors.Is(confirmErr, storage.ErrNotFound) && !errors.Is(confirmErr, storage.ErrCorruptObject) {
			return errors.Join(err, confirmErr)
		}
	}
	return storage.ErrUnavailable
}

func (m *Manager) ackDispatchState(ctx context.Context, task *Task) error {
	copy := *task
	state := *task.Dispatch
	state.Pending, state.Previous = false, nil
	copy.Dispatch = &state
	data, err := json.Marshal(copy)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	entry, err := m.store.Put(ctx, taskKey(task.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: task.version})
	if err != nil {
		return err
	}
	task.version, task.Dispatch = entry.Version, &state
	return nil
}

func (m *Manager) synchronizeDispatchState(ctx context.Context, task *Task) {
	deadline := time.Now().Add(time.Second)
	for {
		err := m.publishDispatchState(ctx, task)
		if errors.Is(err, lease.ErrHeld) {
			if !time.Now().Before(deadline) {
				m.runtimeDispatch(err)
				if latest, readErr := m.Get(ctx, task.ID); readErr == nil {
					*task = latest
				}
				return
			}
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(min(10*time.Millisecond, time.Until(deadline))):
				continue
			}
		}
		if err == nil {
			return
		}
		if !errors.Is(err, lease.ErrHeld) {
			m.runtimeDispatch(err)
		}
		// A successful task commit remains authoritative; its pending index update is replayable.
		if latest, readErr := m.Get(ctx, task.ID); readErr == nil {
			*task = latest
		}
		return
	}
}
