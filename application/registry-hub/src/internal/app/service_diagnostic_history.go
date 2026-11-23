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

func diagnosticBatchEnd(events []diagnostics.Event, offset int) int {
	if offset >= len(events) {
		return offset
	}
	date := diagnostics.EventBatchDate(events[offset])
	size, end := 256, offset
	for end < len(events) && end-offset < diagnostics.BatchMaxEvents && diagnostics.EventBatchDate(events[end]) == date {
		encoded, err := json.Marshal(events[end])
		if err != nil || size+len(encoded)+1 > diagnostics.BatchMaxBytes {
			break
		}
		size += len(encoded) + 1
		end++
	}
	return end
}

func (s *Service) recordDiagnosticBatchGap(ctx context.Context, eventCount int, reason string) error {
	return inventory.Update(ctx, s.store, diagnostics.Prefix+"coverage.json", func(value *diagnosticHistoryCoverage) error {
		value.LastGapAt, value.Reason = time.Now().UTC(), reason
		value.GapCount++
		value.DroppedEvents += uint64(eventCount)
		value.DroppedBatches++
		return nil
	})
}

func (s *Service) runDiagnosticHistory(ctx context.Context) {
	var group sync.WaitGroup
	for _, source := range []struct{ kind, prefix string }{{"tasks", "tasks/state/"}, {"intents", "solidification/blobs/intents/"}, {"work", "background/v1/jobs/"}, {"receipts", "background/v1/receipts/"}, {"downloads", "downloads/v1/"}, {"uploads", "uploads/sessions/"}, {"upload_receipts", diagnostics.Prefix + "upload-receipts/"}, {"scans", "background/v1/source-checkpoints/"}, {"statistics", "background/v1/statistics-checkpoints/"}, {"maintenance", "background/v1/maintenance-checkpoints/"}, {"workers", diagnostics.Prefix + "workers/"}} {
		group.Add(1)
		go func() {
			defer group.Done()
			if source.kind == "receipts" {
				s.runSourceSweepWithState(ctx, "history_receipts", source.prefix, s.archiveDiagnosticReceipt, nil)
				return
			}
			s.runSourceSweep(ctx, "history_"+source.kind, source.prefix, func(ctx context.Context, entry storage.Entry) error {
				if source.kind == "downloads" && !strings.HasSuffix(entry.Key, "/state.json") {
					return nil
				}
				raw, version, err := inventory.Read[json.RawMessage](ctx, s.store, entry.Key)
				if err != nil {
					return err
				}
				if version == "" {
					version = sourceEntryVersion(entry)
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
				return s.collectDiagnosticOutboxVersion(ctx, entry.Key, version, data.DiagnosticOutbox)
			})
		}()
	}
	group.Wait()
}

func (s *Service) handoffDiagnosticOutbox(ctx context.Context, source string, events []diagnostics.Event) error {
	if len(events) == 0 || time.Since(events[len(events)-1].OccurredAt) > s.diagnosticsConfig().EventRetention {
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
	return s.collectDiagnosticOutboxVersion(ctx, source, "", events)
}

func (s *Service) collectDiagnosticOutboxVersion(ctx context.Context, source, sourceVersion string, events []diagnostics.Event) error {
	ctx = context.WithValue(ctx, diagnosticWriteKey{}, true)
	if len(events) == 0 {
		return nil
	}
	key := diagnostics.Prefix + "collected/" + diagnostics.ID(source) + ".json"
	prior, _, err := inventory.Read[eventCollectionReceipt](ctx, s.store, key)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	dirty := false
	if sourceVersion != "" && prior.SourceVersion != sourceVersion {
		if prior.SourceVersion != "" && prior.LastEventID != "" && eventIndex(events, prior.LastEventID) >= 0 && len(prior.Dispositions) > 0 {
			for index := range prior.Dispositions {
				prior.Dispositions[index].SourceVersion = sourceVersion
			}
		} else {
			if prior.LastEventID != "" || prior.SourceVersion != "" {
				gapID, err := s.recordDiagnosticOutboxGap(ctx, source, sourceVersion, nil, "legacy_cursor_without_version_disposition")
				if err != nil {
					return err
				}
				prior.GapIDs = appendUniqueString(prior.GapIDs, gapID)
			}
			prior.LastEventID, prior.Dispositions = "", nil
		}
		prior.SourceVersion, dirty = sourceVersion, true
	}
	offset := 0
	if prior.LastEventID != "" {
		if index := eventIndex(events, prior.LastEventID); index >= 0 {
			offset = index + 1
		} else {
			prior.Truncated = true
			gapID, err := s.recordDiagnosticOutboxGap(ctx, source, prior.SourceVersion, events, "source_outbox_rotation")
			if err != nil {
				return err
			}
			prior.GapIDs = appendUniqueString(prior.GapIDs, gapID)
			prior.LastEventID, prior.Dispositions, offset, dirty = "", nil, 0, true
		}
	}
	if prior.LastEventID == "" && len(events) == diagnostics.OutboxLimit && !prior.Truncated {
		prior.Truncated = true
		gapID, err := s.recordDiagnosticOutboxGap(ctx, source, prior.SourceVersion, events, "source_outbox_may_be_truncated")
		if err != nil {
			return err
		}
		prior.GapIDs = appendUniqueString(prior.GapIDs, gapID)
		dirty = true
	}
	if offset >= len(events) {
		if !dirty {
			return nil
		}
		prior.UpdatedAt = time.Now().UTC()
		return s.saveEventCollectionReceipt(ctx, key, prior, events)
	}
	end := diagnosticBatchEnd(events, offset)
	if end == offset {
		return storage.ErrCorruptObject
	}
	batchEvents := events[offset:end]
	critical := false
	for _, event := range batchEvents {
		critical = critical || diagnosticCriticalEvent(event)
	}
	if timing, ok := ctx.Value(diagnosticSourceWakeTimingKey{}).(*diagnosticSourceWakeTiming); ok && !timing.Recorded {
		s.metrics.recordDiagnosticWakeLatency("admission_to_flush_start", time.Since(timing.AdmittedAt))
		timing.Recorded = true
	}
	batch, err := (diagnostics.BatchLog{Store: s.store, Retention: s.diagnosticsConfig().EventRetention}).Append(ctx, batchEvents, critical)
	disposition := diagnosticEventDisposition{
		SourceKey: source, SourceVersion: prior.SourceVersion, FirstEventID: batchEvents[0].ID,
		LastEventID: batchEvents[len(batchEvents)-1].ID, EventCount: len(batchEvents), Disposition: "archived",
	}
	if errors.Is(err, diagnostics.ErrHistoryCapacity) || errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
		prior.Truncated = true
		gapReason := "batch_budget_capacity"
		if errors.Is(err, diagnostics.ErrBatchHistoryExpired) {
			gapReason = "event_history_expired"
		}
		gapID, gapErr := s.recordDiagnosticOutboxGap(ctx, source, prior.SourceVersion, batchEvents, gapReason)
		if gapErr != nil {
			return gapErr
		}
		prior.GapIDs = appendUniqueString(prior.GapIDs, gapID)
		disposition.Disposition, disposition.Reason, disposition.GapID = "discarded_by_policy", gapReason, gapID
		if err := s.recordDiagnosticBatchGap(ctx, len(batchEvents), gapReason); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		disposition.BatchID = batch.ID
		disposition.BatchSHA256 = batch.ContentSHA256
	}
	prior.LastEventID = batchEvents[len(batchEvents)-1].ID
	prior.Dispositions = appendDisposition(prior.Dispositions, disposition)
	prior.UpdatedAt = time.Now().UTC()
	if prior.Truncated {
		if err := inventory.Update(ctx, s.store, diagnostics.Prefix+"coverage.json", func(value *diagnosticHistoryCoverage) error {
			value.LastGapAt, value.Reason = prior.UpdatedAt, "source_outbox_or_daily_budget_truncated"
			return nil
		}); err != nil {
			return err
		}
	}
	return s.saveEventCollectionReceipt(ctx, key, prior, events)
}

func (s *Service) saveEventCollectionReceipt(ctx context.Context, key string, next eventCollectionReceipt, events []diagnostics.Event) error {
	return inventory.Update(ctx, s.store, key, func(current *eventCollectionReceipt) error {
		if current.SourceVersion == next.SourceVersion && current.LastEventID != "" {
			currentOffset, nextOffset := eventIndex(events, current.LastEventID), eventIndex(events, next.LastEventID)
			if currentOffset > nextOffset {
				next.LastEventID = current.LastEventID
			}
			next.Dispositions = mergeDispositionRanges(current.Dispositions, next.Dispositions, events)
			next.GapIDs = appendUniqueStrings(current.GapIDs, next.GapIDs)
			next.Truncated = next.Truncated || current.Truncated
		}
		*current = next
		return nil
	})
}
