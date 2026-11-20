package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
)

type eventCollectionReceipt struct {
	LastEventID string    `json:"lastEventID"`
	Truncated   bool      `json:"truncated"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type diagnosticHistoryCoverage struct {
	LastGapAt time.Time `json:"lastGapAt"`
	Reason    string    `json:"reason"`
}

type diagnosticReceiptArchive struct {
	SourceKey           string    `json:"sourceKey"`
	SourceVersion       string    `json:"sourceVersion"`
	LastArchivedEventID string    `json:"lastArchivedEventID,omitempty"`
	ProjectionVersion   int       `json:"projectionVersion"`
	NotificationIndexed bool      `json:"notificationIndexed"`
	TransferProjected   bool      `json:"transferProjected"`
	OutboxComplete      bool      `json:"outboxComplete"`
	Complete            bool      `json:"complete"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

const diagnosticReceiptProjectionVersion = 1

func (s *Service) archiveDiagnosticReceipt(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	stateKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(entry.Key) + ".json"
	state, stateVersion, stateErr := inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey)
	if stateErr != nil && !errors.Is(stateErr, storage.ErrNotFound) {
		return stateErr
	}
	if entry.Version != "" && state.SourceVersion == entry.Version && state.ProjectionVersion == diagnosticReceiptProjectionVersion && state.Complete {
		incrementSweepCount(p, "skipped", 1)
		return nil
	}
	current, err := s.store.Head(ctx, entry.Key)
	if err != nil {
		return err
	}
	if sourceEntryVersion(current) != sourceEntryVersion(entry) {
		return storage.ErrConditionFailed
	}
	raw, version, err := inventory.Read[json.RawMessage](ctx, s.store, entry.Key)
	if err != nil {
		return err
	}
	if version == "" {
		version = sourceEntryVersion(entry)
	}
	if entry.Version != "" && version != entry.Version {
		return storage.ErrConditionFailed
	}
	if state.SourceKey != entry.Key || state.SourceVersion != version || state.ProjectionVersion != diagnosticReceiptProjectionVersion {
		state = diagnosticReceiptArchive{SourceKey: entry.Key, SourceVersion: version, ProjectionVersion: diagnosticReceiptProjectionVersion}
	}
	var data struct {
		DiagnosticOutbox []diagnostics.Event `json:"diagnosticOutbox"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	var job workqueue.Job
	isJob := json.Unmarshal(raw, &job) == nil && job.ID != "" && (strings.HasPrefix(entry.Key, "background/v1/receipts/") || strings.HasPrefix(entry.Key, "background/v1/jobs/"))
	if !isJob {
		state.NotificationIndexed = true
	}
	if isJob && !state.NotificationIndexed {
		if err := s.indexNotificationJob(ctx, job); err != nil {
			return err
		}
		state.NotificationIndexed = true
		if err := s.saveDiagnosticReceiptArchive(ctx, stateKey, state, stateVersion); err != nil {
			return err
		}
		if _, stateVersion, err = inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey); err != nil {
			return err
		}
	}
	if isJob {
		association := diagnosticJob(job)
		for i := range data.DiagnosticOutbox {
			event := &data.DiagnosticOutbox[i]
			event.Authority, event.Repository, event.Digest = association.Authority, association.Repository, association.Digest
			event.TaskID, event.IntentID, event.TransferID, event.ImageID = association.TaskID, association.IntentID, association.TransferID, association.ImageID
		}
	}
	if !state.TransferProjected {
		if err := s.projectDiagnosticTransfer(ctx, entry.Key, raw); err != nil {
			return err
		}
		state.TransferProjected = true
		if err := s.saveDiagnosticReceiptArchive(ctx, stateKey, state, stateVersion); err != nil {
			return err
		}
		if _, stateVersion, err = inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey); err != nil {
			return err
		}
	}
	if !state.OutboxComplete {
		for batch := 0; batch < diagnostics.OutboxLimit/4+1; batch++ {
			if err := s.collectDiagnosticOutbox(ctx, entry.Key, data.DiagnosticOutbox); err != nil {
				return err
			}
			if len(data.DiagnosticOutbox) == 0 {
				state.OutboxComplete = true
			} else {
				receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, diagnostics.Prefix+"collected/"+diagnostics.ID(entry.Key)+".json")
				if err != nil {
					return err
				}
				state.LastArchivedEventID = receipt.LastEventID
				state.OutboxComplete = receipt.LastEventID == data.DiagnosticOutbox[len(data.DiagnosticOutbox)-1].ID
			}
			if err := s.saveDiagnosticReceiptArchive(ctx, stateKey, state, stateVersion); err != nil {
				return err
			}
			if _, stateVersion, err = inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey); err != nil {
				return err
			}
			if state.OutboxComplete {
				break
			}
		}
		if !state.OutboxComplete {
			return errSourceBudgetYield
		}
	}
	state.Complete, state.UpdatedAt = true, time.Now().UTC()
	if err := s.saveDiagnosticReceiptArchive(ctx, stateKey, state, stateVersion); err != nil {
		return err
	}
	if data.DiagnosticOutbox != nil {
		incrementSweepCount(p, "archived", int64(len(data.DiagnosticOutbox)))
	}
	return nil
}

func (s *Service) saveDiagnosticReceiptArchive(ctx context.Context, key string, state diagnosticReceiptArchive, version string) error {
	state.UpdatedAt = time.Now().UTC()
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: version == ""}
	if version != "" {
		options = storage.PutOptions{IfMatch: version}
	}
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, _, readErr := inventory.Read[diagnosticReceiptArchive](ctx, s.store, key)
		if readErr == nil && stored.SourceKey == state.SourceKey && stored.SourceVersion == state.SourceVersion &&
			stored.ProjectionVersion == state.ProjectionVersion && stored.LastArchivedEventID == state.LastArchivedEventID &&
			stored.NotificationIndexed == state.NotificationIndexed && stored.TransferProjected == state.TransferProjected &&
			stored.OutboxComplete == state.OutboxComplete && stored.Complete == state.Complete {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}
