package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

const sourceRetryPrefix = "background/v1/source-retries/"
const sourceRetryDuePrefix = "background/v1/source-retry-due/"

type sourceSweepRetry struct {
	SchemaVersion int       `json:"schemaVersion"`
	ID            string    `json:"id"`
	Kind          string    `json:"kind"`
	Partition     string    `json:"partition"`
	SourceKey     string    `json:"sourceKey"`
	SourceVersion string    `json:"sourceVersion"`
	Revision      uint64    `json:"revision"`
	Attempts      int       `json:"attempts"`
	State         string    `json:"state"`
	ErrorClass    string    `json:"errorClass"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type sourceSweepRetryDue struct {
	RetryID  string    `json:"retryID"`
	Revision uint64    `json:"revision"`
	DueAt    time.Time `json:"dueAt"`
}

func sourceSweepUsesDurableRetry(kind string) bool {
	return kind == "history_receipts" || kind == "receipt_cleanup" || strings.HasPrefix(kind, "diagnostic_retention_")
}

func sourceRetryPartition(kind, key string) string {
	if kind == "diagnostic_retention_events" {
		parts := strings.Split(strings.TrimPrefix(key, "diagnostics/v1/events/"), "/")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return "default"
}

func sourceRetryID(kind, partition, key, version string) string {
	return workqueue.ID(kind, partition, key, version)
}

func sourceRetryRecordKey(kind, id string) string {
	return sourceRetryPrefix + kind + "/" + id + ".json"
}

func sourceRetryDueKey(kind string, due time.Time, id string, revision uint64) string {
	return sourceRetryDuePrefix + kind + "/" + due.UTC().Format("20060102T150405.000000000Z") + "/" + id + "-" + fmt.Sprintf("%020d", revision) + ".json"
}

func sourceEntryVersion(entry storage.Entry) string {
	if entry.Version != "" {
		return entry.Version
	}
	return fmt.Sprintf("%d:%d", entry.ModTime, entry.Size)
}

func (s *Service) enqueueSourceSweepRetry(ctx context.Context, kind string, entry storage.Entry, cause error, attempts int, next time.Time) error {
	partition, version := sourceRetryPartition(kind, entry.Key), sourceEntryVersion(entry)
	id := sourceRetryID(kind, partition, entry.Key, version)
	key := sourceRetryRecordKey(kind, id)
	now := time.Now().UTC()
	for try := 0; try < 16; try++ {
		current, storageVersion, err := inventory.Read[sourceSweepRetry](ctx, s.store, key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if err == nil && current.SchemaVersion == 1 && current.ID == id && current.State == "pending" {
			return s.publishSourceRetryDue(ctx, current)
		}
		revision := uint64(1)
		created := now
		if err == nil {
			revision, created = current.Revision+1, current.CreatedAt
		}
		retry := sourceSweepRetry{SchemaVersion: 1, ID: id, Kind: kind, Partition: partition, SourceKey: entry.Key, SourceVersion: version,
			Revision: revision, Attempts: attempts, State: "pending", ErrorClass: diagnosticReason(cause), NextAttemptAt: next.UTC(), CreatedAt: created, UpdatedAt: now}
		data, err := json.Marshal(retry)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: storageVersion == ""}
		if storageVersion != "" {
			options = storage.PutOptions{IfMatch: storageVersion}
		}
		_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			stored, _, readErr := inventory.Read[sourceSweepRetry](ctx, s.store, key)
			if readErr == nil && stored.Revision == retry.Revision && stored.SourceVersion == version {
				return s.publishSourceRetryDue(ctx, stored)
			}
			continue
		}
		if err != nil {
			return err
		}
		return s.publishSourceRetryDue(ctx, retry)
	}
	return storage.ErrUnavailable
}

func (s *Service) publishSourceRetryDue(ctx context.Context, retry sourceSweepRetry) error {
	index := sourceSweepRetryDue{RetryID: retry.ID, Revision: retry.Revision, DueAt: retry.NextAttemptAt}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	key := sourceRetryDueKey(retry.Kind, index.DueAt, retry.ID, retry.Revision)
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrUnknownCommit) {
		stored, _, readErr := inventory.Read[sourceSweepRetryDue](ctx, s.store, key)
		if readErr == nil && stored == index {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}

func (s *Service) repairSourceRetryIndexes(ctx context.Context, kind string, p *sourceSweepCheckpoint, limit int) error {
	if !sourceSweepUsesDurableRetry(kind) {
		return nil
	}
	prefix := sourceRetryPrefix + kind + "/"
	for count := 0; count < limit; count++ {
		entry, found, err := p.RetryIndexCursor.Peek(ctx, s.store, prefix, limit)
		if err != nil {
			return err
		}
		if !found {
			if p.RetryIndexCursor.Done {
				p.RetryIndexCursor = inventory.Cursor{}
			}
			return nil
		}
		retry, _, err := inventory.Read[sourceSweepRetry](ctx, s.store, entry.Key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				p.RetryIndexCursor.Advance()
				continue
			}
			return err
		}
		if retry.SchemaVersion == 1 && retry.Kind == kind && retry.State == "pending" {
			if err := s.publishSourceRetryDue(ctx, retry); err != nil {
				return err
			}
		}
		p.RetryIndexCursor.Advance()
	}
	if p.RetryIndexCursor.Done {
		p.RetryIndexCursor = inventory.Cursor{}
	}
	return nil
}

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
