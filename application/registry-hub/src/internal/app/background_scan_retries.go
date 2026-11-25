package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"registry-hub/internal/inventory"
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
	SourceSize    *int64    `json:"sourceSize,omitempty"`
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
	return kind == "history_receipts" || kind == "receipt_cleanup" || kind == "receipt_cleanup_preview" || strings.HasPrefix(kind, "diagnostic_retention_")
}

func sourceSweepRetryKinds() []string {
	var kinds []string
	for _, kind := range historyCleanupRetentionWorkerKinds() {
		if sourceSweepUsesDurableRetry(kind) {
			kinds = append(kinds, kind)
		}
	}
	return kinds
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

func (s *Service) sourceSweepRetryPending(ctx context.Context, kind string, entry storage.Entry) (bool, error) {
	if !sourceSweepUsesDurableRetry(kind) {
		return false, nil
	}
	// The retry dispatcher owns this version, including after its due time.
	id := sourceRetryID(kind, sourceRetryPartition(kind, entry.Key), entry.Key, sourceEntryIdentity(entry))
	_, err := s.store.Head(ctx, sourceRetryRecordKey(kind, id))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
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

func sourceEntryIdentity(entry storage.Entry) string {
	if entry.MetadataFingerprint != "" {
		return entry.MetadataFingerprint
	}
	return sourceEntryVersion(entry)
}

func sameSourceEntry(current, listed storage.Entry) bool {
	if current.MetadataFingerprint != "" && listed.MetadataFingerprint != "" {
		return current.MetadataFingerprint == listed.MetadataFingerprint
	}
	return sourceEntryVersion(current) == sourceEntryVersion(listed)
}

func (s *Service) enqueueSourceSweepRetry(ctx context.Context, kind string, entry storage.Entry, cause error, attempts int, next time.Time) error {
	partition, version := sourceRetryPartition(kind, entry.Key), sourceEntryIdentity(entry)
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
		if terminalHistoryIndexBackfillWorker(kind) {
			retry.SourceSize = &entry.Size
		}
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
			s.signalSourceRetry(retry.Kind)
			return nil
		}
		return errors.Join(err, readErr)
	}
	if err == nil {
		s.signalSourceRetry(retry.Kind)
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
