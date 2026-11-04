package app

import (
	"context"
	"errors"
	"registry-hub/internal/tasks"
)

func (s *solidifier) WithTerminalChange(ctx context.Context, id string, run func(context.Context) error) error {
	return s.service.records.WithBlobDemandLock(ctx, id, run)
}

func (s *solidifier) TaskTerminal(ctx context.Context, task *tasks.Task) error {
	if !isTerminalTask(task.Status) || task.TerminalCleanupComplete {
		return nil
	}
	if hasLegacyRecoveryBlocker(*task) && !hasTaskProtectionReferences(*task) && !safeLegacyTerminalTask(*task) {
		return errors.New("legacy task recovery blocker cannot be cleared from terminal task state")
	}
	current, err := s.tasks.PrepareTerminalCleanup(ctx, task.ID)
	if err != nil {
		return err
	}
	*task = current
	if !isTerminalTask(current.Status) || current.TerminalCleanupComplete {
		return nil
	}
	work := terminalCleanupWork{TaskID: current.ID, CleanupID: current.TerminalCleanupID, RoundID: current.RoundID, Generation: current.Generation, Status: current.Status, Authority: current.Authority, Repository: current.Repository, Digest: current.RootDigest}
	if current.Kind == tasks.BlobKind {
		work.Digest = current.BlobDigest
	}
	return s.service.enqueueProjection(ctx, "terminal_cleanup", current.ID+"/"+current.TerminalCleanupID, work, current.CreatedAt)
}
