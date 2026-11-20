package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *Service) dispatchDueSourceSweepRetry(ctx context.Context, holder lease.Lease, kind string, p *sourceSweepCheckpoint, step sourceSweepStep) (bool, error) {
	entries, _, err := s.store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 32)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		index, version, readErr := inventory.Read[sourceSweepRetryDue](ctx, s.store, entry.Key)
		if errors.Is(readErr, storage.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		if index.DueAt.After(now) {
			break
		}
		retry, recordVersion, readErr := inventory.Read[sourceSweepRetry](ctx, s.store, sourceRetryRecordKey(kind, index.RetryID))
		if errors.Is(readErr, storage.ErrNotFound) || readErr == nil && (retry.State != "pending" || retry.Revision != index.Revision || retry.Kind != kind) {
			_ = s.store.DeleteIfMatch(ctx, entry.Key, version)
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		if retry.NextAttemptAt.After(now) {
			continue
		}
		manager := lease.NewManager(s.store)
		claim, claimErr := manager.Acquire(ctx, "background/source-retries/"+retry.ID, s.instanceID+"/"+requestID(), time.Minute)
		if errors.Is(claimErr, lease.ErrHeld) {
			return false, nil
		}
		if claimErr != nil {
			return false, claimErr
		}
		if err := manager.Validate(ctx, holder); err != nil {
			_ = manager.Release(context.Background(), claim)
			return false, err
		}
		current, currentVersion, currentErr := inventory.Read[sourceSweepRetry](ctx, s.store, sourceRetryRecordKey(kind, retry.ID))
		if currentErr != nil || currentVersion != recordVersion || current.Revision != retry.Revision {
			_ = manager.Release(context.Background(), claim)
			if currentErr != nil && !errors.Is(currentErr, storage.ErrNotFound) {
				return false, currentErr
			}
			continue
		}
		p.Status.DispatchLagSeconds = max(0, now.Sub(retry.NextAttemptAt).Seconds())
		p.Status.Operations++
		s.updateWorker(kind, func(status *workerStatus) { status.State = "retrying" })
		source, sourceErr := s.store.Head(ctx, retry.SourceKey)
		if errors.Is(sourceErr, storage.ErrNotFound) || sourceErr == nil && sourceEntryVersion(source) != retry.SourceVersion {
			return true, s.completeSourceRetry(ctx, manager, claim, entry, version, retry, recordVersion)
		}
		if sourceErr != nil {
			_ = manager.Release(context.Background(), claim)
			return false, sourceErr
		}
		runCtx := inventory.WithWriteGuard(ctx, func(check context.Context) error {
			if err := manager.Validate(check, holder); err != nil {
				return err
			}
			return manager.Validate(check, claim)
		})
		stepErr := step(runCtx, source, p)
		if errors.Is(stepErr, errSourceBudgetYield) || ctx.Err() != nil && pureContextBudget(stepErr) {
			_ = manager.Release(context.Background(), claim)
			return true, nil
		}
		if s.sourceDependencyWait(ctx, kind, p, stepErr) {
			_ = manager.Release(context.Background(), claim)
			return true, nil
		}
		if stepErr == nil || errors.Is(stepErr, storage.ErrNotFound) || errors.Is(stepErr, storage.ErrConditionFailed) {
			return true, s.completeSourceRetry(ctx, manager, claim, entry, version, retry, recordVersion)
		}
		return true, s.rescheduleSourceRetry(ctx, manager, claim, entry, version, retry, recordVersion, stepErr)
	}
	return false, nil
}

func (s *Service) completeSourceRetry(ctx context.Context, manager *lease.Manager, claim lease.Lease, dueEntry storage.Entry, dueVersion string, retry sourceSweepRetry, recordVersion string) error {
	if err := manager.Validate(ctx, claim); err != nil {
		return err
	}
	if err := s.store.DeleteIfMatch(ctx, sourceRetryRecordKey(retry.Kind, retry.ID), recordVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	if err := s.store.DeleteIfMatch(ctx, dueEntry.Key, dueVersion); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	_ = manager.Release(context.Background(), claim)
	return nil
}

func (s *Service) rescheduleSourceRetry(ctx context.Context, manager *lease.Manager, claim lease.Lease, dueEntry storage.Entry, dueVersion string, retry sourceSweepRetry, recordVersion string, cause error) error {
	if err := manager.Validate(ctx, claim); err != nil {
		return err
	}
	retry.Revision++
	retry.Attempts++
	retry.ErrorClass = diagnosticReason(cause)
	retry.NextAttemptAt = time.Now().UTC().Add(min(time.Second*time.Duration(1<<min(retry.Attempts, 8)), 5*time.Minute))
	retry.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(retry)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = s.store.Put(ctx, sourceRetryRecordKey(retry.Kind, retry.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: recordVersion})
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, _, readErr := inventory.Read[sourceSweepRetry](ctx, s.store, sourceRetryRecordKey(retry.Kind, retry.ID))
		if readErr == nil && stored.Revision == retry.Revision {
			err = nil
		}
	}
	if err != nil {
		return err
	}
	if err := s.publishSourceRetryDue(ctx, retry); err != nil {
		return err
	}
	if err := manager.Validate(ctx, claim); err != nil {
		return err
	}
	if err := s.store.DeleteIfMatch(ctx, dueEntry.Key, dueVersion); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	_ = manager.Release(context.Background(), claim)
	return nil
}

func (s *Service) updateSourceRetryStatus(ctx context.Context, kind string, p *sourceSweepCheckpoint) error {
	p.Status.PendingRetries = 0
	p.Status.OldestDueRetryAt = time.Time{}
	p.Status.NextRetryAt = time.Time{}
	token := ""
	for {
		entries, next, err := s.store.List(ctx, sourceRetryPrefix+kind+"/", token, 1000)
		if err != nil {
			return err
		}
		p.Status.PendingRetries += int64(len(entries))
		if next == "" {
			break
		}
		if next == token {
			return storage.ErrUnavailable
		}
		token = next
	}
	dueEntries, _, err := s.store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 1000)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, entry := range dueEntries {
		index, _, err := inventory.Read[sourceSweepRetryDue](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		retry, _, err := inventory.Read[sourceSweepRetry](ctx, s.store, sourceRetryRecordKey(kind, index.RetryID))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if retry.State != "pending" || retry.Revision != index.Revision || retry.NextAttemptAt != index.DueAt {
			continue
		}
		p.Status.NextRetryAt = retry.NextAttemptAt
		if !retry.NextAttemptAt.After(now) {
			p.Status.OldestDueRetryAt = retry.NextAttemptAt
		}
		break
	}
	return nil
}

func importSourceSweepBackoff(ctx context.Context, s *Service, kind string, p *sourceSweepCheckpoint, limit int) error {
	if !sourceSweepUsesDurableRetry(kind) {
		return nil
	}
	count := 0
	for key, backoff := range p.Backoff {
		if count == limit {
			break
		}
		index := strings.LastIndexByte(key, '\x00')
		if index <= 0 {
			delete(p.Backoff, key)
			continue
		}
		entry := storage.Entry{Key: key[:index], Version: key[index+1:]}
		if err := s.enqueueSourceSweepRetry(ctx, kind, entry, errors.New("migrated_source_backoff"), max(1, backoff.Retries), backoff.Next); err != nil {
			return err
		}
		delete(p.Backoff, key)
		count++
	}
	return nil
}

func dueSourceRetryExists(ctx context.Context, store storage.Store, kind string) (bool, error) {
	entries, _, err := store.List(ctx, sourceRetryDuePrefix+kind+"/", "", 1)
	if err != nil || len(entries) == 0 {
		return false, err
	}
	index, _, err := inventory.Read[sourceSweepRetryDue](ctx, store, entries[0].Key)
	if errors.Is(err, storage.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !index.DueAt.After(time.Now()), nil
}
