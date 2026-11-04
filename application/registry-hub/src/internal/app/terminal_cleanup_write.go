package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

type terminalWriteScopeKey struct{}

var errTerminalBudgetYield = errors.New("terminal cleanup yielded before issuing a mutation")
var errTerminalCheckpointPending = errors.New("confirmed terminal mutation requires a checkpoint")

type terminalMutation struct {
	Operation string `json:"operation"`
	Key       string `json:"key"`
	Version   string `json:"version,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type terminalWriteScope struct {
	service         *Service
	job             *workqueue.Job
	work            terminalCleanupWork
	progress        *terminalCleanupProgress
	before          terminalCleanupProgress
	parent          context.Context
	manager         *lease.Manager
	lease           lease.Lease
	epoch           uint64
	reserve         time.Duration
	confirmed       bool
	checkpointError error
}

func (s *terminalWriteScope) guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.manager.Validate(ctx, s.lease); err != nil {
		if errors.Is(err, lease.ErrLost) || errors.Is(err, storage.ErrNotFound) {
			return errors.Join(storage.ErrLeaseLost, err)
		}
		return err
	}
	if err := (workqueue.Queue{Store: s.service.store}).Validate(ctx, *s.job); err != nil {
		return err
	}
	task, err := s.service.tasks.Get(ctx, s.work.TaskID)
	if err != nil {
		return err
	}
	if !s.work.matches(task) {
		return storage.ErrLeaseLost
	}
	gate, err := readRecoveryGate(ctx, s.service.store)
	if err != nil {
		return err
	}
	if gate.Epoch != s.epoch || gate.State != "open" || gate.RecoveryBarrier {
		return storage.ErrLeaseLost
	}
	return nil
}

func (s *terminalWriteScope) journal(pending *terminalMutation) error {
	ctx, cancel := context.WithTimeout(s.parent, 5*time.Second)
	defer cancel()
	if err := s.guard(ctx); err != nil {
		return err
	}
	checkpoint := s.before
	checkpoint.Pending = pending
	if err := (workqueue.Queue{Store: s.service.store}).Checkpoint(ctx, s.job, checkpoint); err != nil {
		return err
	}
	s.progress.Pending = pending
	return nil
}

func (s *terminalWriteScope) begin(ctx context.Context, pending terminalMutation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < s.reserve {
		return errTerminalBudgetYield
	}
	if s.progress.Pending != nil {
		if s.confirmed {
			return errTerminalCheckpointPending
		}
		return storage.ErrUnknownCommit
	}
	s.confirmed, s.checkpointError = false, nil
	if err := s.journal(&pending); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		// The request was never issued; its journal can be cleared safely.
		return errors.Join(err, s.journal(nil))
	}
	return nil
}

func terminalWriteUncertain(err error) bool {
	return err != nil && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrPermissionDenied) && !errors.Is(err, storage.ErrInvalidKey)
}

func (s *terminalWriteScope) finish(store storage.Store, entry storage.Entry, cause error) (storage.Entry, error) {
	if cause != nil && terminalWriteUncertain(cause) {
		ctx, cancel := context.WithTimeout(s.parent, 5*time.Second)
		defer cancel()
		if err := s.guard(ctx); err != nil {
			return entry, errors.Join(cause, err)
		}
		confirmed, actual, err := confirmTerminalMutation(ctx, store, s.progress.Pending)
		if err != nil || !confirmed {
			s.progress.Confirmation = "unresolved"
			return entry, errors.Join(storage.ErrUnknownCommit, cause, err)
		}
		entry, cause = actual, nil
		s.progress.Confirmation = "readback_confirmed"
	}
	s.confirmed = true
	if err := s.journal(nil); err != nil {
		s.checkpointError = err
		s.progress.Confirmation = "known_result_checkpoint_pending"
		// Report the confirmed storage result; the worker retains the journal and retries its checkpoint.
		return entry, cause
	}
	return entry, cause
}

func confirmTerminalMutation(ctx context.Context, store storage.Store, pending *terminalMutation) (bool, storage.Entry, error) {
	if pending == nil {
		return true, storage.Entry{}, nil
	}
	if pending.Operation == "delete" {
		entry, err := store.Head(ctx, pending.Key)
		return errors.Is(err, storage.ErrNotFound), entry, ignoreTerminalNotFound(err)
	}
	object, err := store.Get(ctx, pending.Key)
	if err != nil {
		return false, storage.Entry{}, err
	}
	hash := sha256.New()
	_, readErr := io.Copy(hash, object.Body)
	closeErr := object.Body.Close()
	return hex.EncodeToString(hash.Sum(nil)) == pending.SHA256, storage.Entry{Key: pending.Key, Version: object.Version}, errors.Join(readErr, closeErr)
}

func ignoreTerminalNotFound(err error) error {
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (s *terminalWriteScope) resume() error {
	s.before = *s.progress
	ctx, cancel := context.WithTimeout(s.parent, 5*time.Second)
	defer cancel()
	if err := s.guard(ctx); err != nil {
		return err
	}
	confirmed, _, err := confirmTerminalMutation(ctx, s.service.store, s.progress.Pending)
	if err != nil || !confirmed {
		return errors.Join(storage.ErrUnknownCommit, err)
	}
	s.progress.Confirmation = "restart_readback_confirmed"
	s.confirmed = true
	return s.journal(nil)
}

func (s *terminalWriteScope) recover(cause error) error {
	ctx, cancel := context.WithTimeout(s.parent, 5*time.Second)
	defer cancel()
	if err := s.guard(ctx); err != nil {
		s.service.gate.mu.Lock()
		s.service.gate.failClosedLocked("terminal_cleanup_confirmation_unknown")
		s.service.gate.mu.Unlock()
		return errors.Join(cause, err)
	}
	kind, reason := "terminal_cleanup", "terminal_cleanup_write_unknown"
	if errors.Is(cause, errTerminalRemoteUnknown) {
		kind, reason = "unknown_remote_operation", "terminal_remote_operation_unknown"
	}
	ctx = withRecoveryTrigger(ctx, kind, reason, s.work.TaskID, s.job.ID, s.job.AttemptID, cause)
	ctx = context.WithValue(ctx, recoveryWriteGuardKey{}, recoveryWriteGuard(s.guard))
	err := s.service.gate.SetRecoveryBarrier(ctx, reason)
	s.progress.RecoveryID = s.service.gate.State().RecoveryID
	s.progress.YieldReason = "protection_recovery_required"
	return errors.Join(storage.ErrUnknownCommit, cause, err)
}

func terminalMutationKnown(ctx context.Context) bool {
	scope, ok := ctx.Value(terminalWriteScopeKey{}).(*terminalWriteScope)
	return ok && (scope.progress.Pending == nil || scope.confirmed)
}
