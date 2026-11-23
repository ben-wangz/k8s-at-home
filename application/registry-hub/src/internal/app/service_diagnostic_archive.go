package app

import (
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
	SourceVersion string                       `json:"sourceVersion,omitempty"`
	LastEventID   string                       `json:"lastEventID"`
	Truncated     bool                         `json:"truncated"`
	Dispositions  []diagnosticEventDisposition `json:"dispositions,omitempty"`
	GapIDs        []string                     `json:"gapIDs,omitempty"`
	UpdatedAt     time.Time                    `json:"updatedAt"`
}

type diagnosticEventDisposition struct {
	SourceKey     string `json:"sourceKey"`
	SourceVersion string `json:"sourceVersion"`
	FirstEventID  string `json:"firstEventID"`
	LastEventID   string `json:"lastEventID"`
	EventCount    int    `json:"eventCount"`
	Disposition   string `json:"disposition"`
	Reason        string `json:"reason,omitempty"`
	BatchID       string `json:"batchID,omitempty"`
	BatchSHA256   string `json:"batchSHA256,omitempty"`
	GapID         string `json:"gapID,omitempty"`
}

type diagnosticCoverageGapRecord struct {
	ID            string    `json:"id"`
	SourceKey     string    `json:"sourceKey,omitempty"`
	SourceVersion string    `json:"sourceVersion,omitempty"`
	FirstEventID  string    `json:"firstEventID,omitempty"`
	LastEventID   string    `json:"lastEventID,omitempty"`
	EventCount    int       `json:"eventCount"`
	Reason        string    `json:"reason"`
	OccurredAt    time.Time `json:"occurredAt"`
}

type diagnosticHistoryCoverage struct {
	LastGapAt      time.Time `json:"lastGapAt"`
	Reason         string    `json:"reason"`
	GapCount       uint64    `json:"gapCount,omitempty"`
	DroppedEvents  uint64    `json:"droppedEvents,omitempty"`
	DroppedBatches uint64    `json:"droppedBatches,omitempty"`
}

type diagnosticReceiptArchive struct {
	SourceKey                 string    `json:"sourceKey"`
	SourceVersion             string    `json:"sourceVersion"`
	SourceFingerprint         string    `json:"sourceFingerprint,omitempty"`
	LastArchivedEventID       string    `json:"lastArchivedEventID,omitempty"`
	ProjectionVersion         int       `json:"projectionVersion"`
	NotificationIndexed       bool      `json:"notificationIndexed"`
	TransferProjected         bool      `json:"transferProjected"`
	ExecutionFactProjected    bool      `json:"executionFactProjected"`
	ExecutionSourcesVerified  bool      `json:"executionSourcesVerified"`
	ExecutionVerificationDone bool      `json:"executionVerificationDone"`
	HistoryProjected          bool      `json:"historyProjected"`
	OutboxComplete            bool      `json:"outboxComplete"`
	OutboxDispositionVersion  int       `json:"outboxDispositionVersion,omitempty"`
	Complete                  bool      `json:"complete"`
	UpdatedAt                 time.Time `json:"updatedAt"`
}

const diagnosticReceiptProjectionVersion = 5

func (s *Service) archiveDiagnosticReceipt(ctx context.Context, entry storage.Entry, p *sourceSweepCheckpoint) error {
	stateKey := diagnostics.Prefix + "archive-projections/" + diagnostics.ID(entry.Key) + ".json"
	state, stateVersion, stateErr := inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey)
	if stateErr != nil && !errors.Is(stateErr, storage.ErrNotFound) {
		return stateErr
	}
	kind, _, receipt := receiptWorkIdentity(entry.Key)
	executionReady := !receipt || kind != "execution" || state.ExecutionVerificationDone
	if diagnosticReceiptSourceMatches(state, entry) && state.ProjectionVersion == diagnosticReceiptProjectionVersion && state.Complete && state.ExecutionFactProjected && executionReady {
		incrementSweepCount(p, "skipped", 1)
		return nil
	}
	current, err := s.store.Head(ctx, entry.Key)
	if err != nil {
		return err
	}
	if !sameSourceEntry(current, entry) {
		return storage.ErrConditionFailed
	}
	entry = current
	sourceFingerprint := sourceEntryIdentity(entry)
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
	if state.SourceKey != entry.Key || state.SourceVersion != version || state.SourceFingerprint != sourceFingerprint || state.ProjectionVersion != diagnosticReceiptProjectionVersion {
		state = diagnosticReceiptArchive{SourceKey: entry.Key, SourceVersion: version, SourceFingerprint: sourceFingerprint, ProjectionVersion: diagnosticReceiptProjectionVersion}
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
	isExecutionReceipt := isJob && strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/") && job.Kind == "execution"
	if !isExecutionReceipt {
		state.ExecutionFactProjected, state.ExecutionSourcesVerified, state.ExecutionVerificationDone = true, true, true
	} else if !state.ExecutionFactProjected && job.State == "done" {
		var fact inventoryExecutionFact
		if json.Unmarshal(job.Payload, &fact) != nil {
			return storage.ErrCorruptObject
		}
		if err := s.indexExecutionSource(ctx, job, fact); err != nil {
			return err
		}
		if err := s.projectCompactExecutionFact(ctx, entry, job, fact); err != nil {
			return err
		}
		state.ExecutionFactProjected = true
		state.ExecutionSourcesVerified, err = s.verifyExecutionSourceLinks(ctx, job, fact)
		if err != nil {
			return err
		}
		state.ExecutionVerificationDone = true
		if err := s.saveDiagnosticReceiptArchive(ctx, stateKey, state, stateVersion); err != nil {
			return err
		}
		if _, stateVersion, err = inventory.Read[diagnosticReceiptArchive](ctx, s.store, stateKey); err != nil {
			return err
		}
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
			if err := s.collectDiagnosticOutboxVersion(ctx, entry.Key, version, data.DiagnosticOutbox); err != nil {
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
				state.OutboxComplete = diagnosticOutboxSettled(entry.Key, version, data.DiagnosticOutbox, receipt)
			}
			if state.OutboxComplete {
				state.OutboxDispositionVersion = 1
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
	if !isJob || !strings.HasPrefix(entry.Key, workqueue.Prefix+"receipts/") || job.State != "done" {
		state.HistoryProjected = true
	} else if !state.HistoryProjected {
		state.HistoryProjected, err = s.projectTerminalWorkHistory(ctx, entry.Key, version, entry, job)
		if err != nil {
			return err
		}
	}
	if !state.HistoryProjected {
		return errSourceBudgetYield
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

func diagnosticReceiptSourceMatches(state diagnosticReceiptArchive, entry storage.Entry) bool {
	if entry.MetadataFingerprint != "" {
		return state.SourceFingerprint == entry.MetadataFingerprint
	}
	if entry.Version != "" {
		return state.SourceVersion == entry.Version
	}
	return state.SourceFingerprint == sourceEntryIdentity(entry)
}
