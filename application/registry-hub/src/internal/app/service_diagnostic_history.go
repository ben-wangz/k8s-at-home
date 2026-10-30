package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"sync"
	"time"
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

func (s *Service) runDiagnosticHistory(ctx context.Context) {
	var group sync.WaitGroup
	for _, source := range []struct{ kind, prefix string }{{"tasks", "tasks/state/"}, {"intents", "solidification/blobs/intents/"}, {"work", "background/v1/jobs/"}, {"receipts", "background/v1/receipts/"}, {"downloads", "downloads/v1/"}, {"uploads", "uploads/sessions/"}, {"upload_receipts", diagnostics.Prefix + "upload-receipts/"}, {"scans", "background/v1/source-checkpoints/"}, {"statistics", "background/v1/statistics-checkpoints/"}, {"maintenance", "background/v1/maintenance-checkpoints/"}, {"workers", diagnostics.Prefix + "workers/"}} {
		group.Add(1)
		go func() {
			defer group.Done()
			s.runSourceSweep(ctx, "history_"+source.kind, source.prefix, func(ctx context.Context, entry storage.Entry) error {
				if source.kind == "downloads" && !strings.HasSuffix(entry.Key, "/state.json") {
					return nil
				}
				raw, _, err := inventory.Read[json.RawMessage](ctx, s.store, entry.Key)
				if err != nil {
					return err
				}
				var data struct {
					DiagnosticOutbox []diagnostics.Event `json:"diagnosticOutbox"`
				}
				if json.Unmarshal(raw, &data) != nil {
					return nil
				}
				if source.kind == "work" || source.kind == "receipts" {
					var job workqueue.Job
					if json.Unmarshal(raw, &job) == nil {
						if err := s.indexNotificationJob(ctx, job); err != nil {
							return err
						}
						association := diagnosticJob(job)
						for i := range data.DiagnosticOutbox {
							event := &data.DiagnosticOutbox[i]
							event.Authority, event.Repository, event.Digest = association.Authority, association.Repository, association.Digest
							event.TaskID, event.IntentID, event.TransferID, event.ImageID = association.TaskID, association.IntentID, association.TransferID, association.ImageID
						}
					}
				}
				if err := s.projectDiagnosticTransfer(ctx, entry.Key, raw); err != nil {
					return err
				}
				return s.collectDiagnosticOutbox(ctx, entry.Key, data.DiagnosticOutbox)
			})
		}()
	}
	group.Wait()
}

func (s *Service) handoffDiagnosticOutbox(ctx context.Context, source string, events []diagnostics.Event) error {
	if len(events) == 0 || time.Since(events[len(events)-1].OccurredAt) > diagnostics.Retention {
		return nil
	}
	if err := s.collectDiagnosticOutbox(ctx, source, events); err != nil {
		return err
	}
	receipt, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, diagnostics.Prefix+"collected/"+diagnostics.ID(source)+".json")
	if err != nil {
		return err
	}
	if receipt.LastEventID != events[len(events)-1].ID {
		return storage.ErrConditionFailed
	}
	return nil
}

func (s *Service) collectDiagnosticOutbox(ctx context.Context, source string, events []diagnostics.Event) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	if len(events) == 0 {
		return nil
	}
	key := diagnostics.Prefix + "collected/" + diagnostics.ID(source) + ".json"
	prior, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	offset := 0
	if prior.LastEventID != "" {
		found := false
		for index, event := range events {
			if event.ID == prior.LastEventID {
				offset = index + 1
				found = true
				break
			}
		}
		if !found {
			prior.Truncated = true
		}
	}
	if prior.LastEventID == "" && len(events) == diagnostics.OutboxLimit {
		prior.Truncated = true
	}
	end := min(offset+4, len(events))
	for _, event := range events[offset:end] {
		err := (diagnostics.Log{Store: s.store}).Append(ctx, event)
		if errors.Is(err, diagnostics.ErrHistoryCapacity) {
			prior.Truncated = true
		} else if err != nil {
			return err
		}
		prior.LastEventID = event.ID
	}
	if offset == end {
		return nil
	}
	prior.UpdatedAt = time.Now().UTC()
	if prior.Truncated {
		if err := inventory.Update(ctx, s.store, diagnostics.Prefix+"coverage.json", func(value *diagnosticHistoryCoverage) error {
			value.LastGapAt, value.Reason = prior.UpdatedAt, "source_outbox_or_daily_budget_truncated"
			return nil
		}); err != nil {
			return err
		}
	}
	return inventory.Update(ctx, s.store, key, func(value *eventCollectionReceipt) error { *value = prior; return nil })
}
