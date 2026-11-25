package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"strings"
	"time"
)

func historyCleanupRangePrefixes(category string, scope historyCleanupRange) []string {
	if !scope.From.Before(scope.To) {
		return nil
	}
	switch category {
	case "diagnostic_events":
		var prefixes []string
		for day := scope.From.Truncate(24 * time.Hour); day.Before(scope.To); day = day.Add(24 * time.Hour) {
			date := day.Format("20060102")
			prefixes = append(prefixes, diagnostics.Prefix+"events/"+date+"/", diagnostics.BatchPrefix+date+"/")
		}
		return prefixes
	case "terminal_work_history":
		return []string{terminalWorkHistoryOrderPrefix}
	case "terminal_transfer_history":
		return []string{terminalTransferHistoryOrderPrefix}
	case "storage_snapshot_history":
		return []string{storageInventoryPrefix + "snapshots/"}
	case "full_work_receipts":
		return []string{workqueue.Prefix + "receipts/"}
	case "diagnostic_collection_receipts":
		return []string{diagnostics.Prefix + "collected/"}
	case "transfer_observations":
		return []string{diagnostics.Prefix + "transfers/", diagnostics.Prefix + "transfer-sessions/", diagnostics.Prefix + "upload-receipts/", diagnostics.Prefix + "transfer-progress/"}
	default:
		return nil
	}
}

func (s *Service) scanHistoryCleanupRangeBatch(ctx context.Context, preview historyCleanupRangePreview, progress *historyCleanupRangeProgress, prefixes []string) (bool, error) {
	limit := historyCleanupRangeBatchSize
	for examined := 0; examined < limit && len(progress.Chunk) < historyCleanupBatchSize; {
		if progress.PrefixIndex >= len(prefixes) {
			return true, nil
		}
		entry, ok, err := progress.Cursor.Peek(ctx, s.store, prefixes[progress.PrefixIndex], historyCleanupBatchSize)
		if err != nil {
			return false, err
		}
		if progress.Snapshot.Pending && (!ok && progress.Cursor.Done || ok && !strings.HasPrefix(entry.Key, storageInventoryPrefix+"snapshots/"+progress.SnapshotID+"/")) {
			examined++
			added, err := appendHistoryCleanupCandidate(progress, *progress.Snapshot.Manifest)
			if err != nil || !added {
				return !added, err
			}
			progress.Snapshot.Pending = false
			if progress.SegmentObjects >= historyCleanupSegmentItems || progress.SegmentBytes >= historyCleanupSegmentBytes {
				return true, nil
			}
			continue
		}
		if !ok {
			if !progress.Cursor.Done {
				return false, nil
			}
			progress.PrefixIndex++
			progress.Cursor = inventory.Cursor{}
			continue
		}
		if historyCleanupRangePassed(preview.Category, entry.Key, preview.EffectiveRange.From) {
			progress.PrefixIndex = len(prefixes)
			progress.Cursor = inventory.Cursor{Done: true}
			return true, nil
		}
		examined++
		if entry.Size > historyCleanupSegmentBytes {
			progress.Counts.Oversized++
			progress.Counts.OversizedBytes += entry.Size
			progress.Counts.SkipReasons["object_exceeds_segment_budget"]++
			progress.Scanned++
			progress.Counts.Scanned++
			progress.Cursor.Advance()
			continue
		}
		candidate, inRange, err := s.classifyHistoryCleanupRangeEntry(ctx, preview, entry, progress)
		if err != nil {
			return false, err
		}
		if inRange {
			if candidate.Eligible {
				added, err := appendHistoryCleanupCandidate(progress, candidate.Reference)
				if err != nil {
					return false, err
				}
				if !added {
					return true, nil
				}
			} else if candidate.Reason != "outside_range" {
				progress.Counts.Protected++
				progress.Counts.ProtectedBytes += entry.Size
				progress.Counts.Skipped++
				progress.Counts.SkipReasons[candidate.Reason]++
			}
		}
		progress.Scanned++
		progress.Counts.Scanned++
		progress.Cursor.Advance()
		if progress.SegmentObjects >= historyCleanupSegmentItems || progress.SegmentBytes >= historyCleanupSegmentBytes {
			return true, nil
		}
	}
	return false, nil
}

func historyCleanupRangePassed(category, key string, from time.Time) bool {
	prefix := ""
	switch category {
	case "terminal_work_history":
		prefix = terminalWorkHistoryOrderPrefix
	case "terminal_transfer_history":
		prefix = terminalTransferHistoryOrderPrefix
	default:
		return false
	}
	at, ok := historyOrderTime(prefix, key)
	return ok && at.Before(from)
}

type historyCleanupRangeCandidate struct {
	Eligible  bool
	Reason    string
	Reference historyCleanupCandidateRef
}

func appendHistoryCleanupCandidate(progress *historyCleanupRangeProgress, candidate historyCleanupCandidateRef) (bool, error) {
	if candidate.Size > historyCleanupSegmentBytes {
		progress.Counts.Oversized++
		progress.Counts.OversizedBytes += candidate.Size
		progress.Counts.SkipReasons["object_exceeds_segment_budget"]++
		return true, nil
	}
	bytes, objects := candidate.Size, 1
	if candidate.Index != nil {
		bytes += candidate.Index.Size
		objects++
	}
	if bytes > historyCleanupSegmentBytes {
		progress.Counts.Oversized += int64(objects)
		progress.Counts.OversizedBytes += bytes
		progress.Counts.SkipReasons["candidate_exceeds_segment_budget"]++
		return true, nil
	}
	if progress.SegmentObjects+objects > historyCleanupSegmentItems || progress.SegmentBytes+bytes > historyCleanupSegmentBytes {
		return false, nil
	}
	progress.Chunk = append(progress.Chunk, candidate)
	progress.SegmentItems++
	progress.SegmentObjects += objects
	progress.SegmentBytes += bytes
	progress.Counts.Eligible += int64(objects)
	progress.Counts.EligibleBytes += bytes
	if !candidate.At.IsZero() && (progress.Counts.OldestEligibleAt == nil || candidate.At.Before(*progress.Counts.OldestEligibleAt)) {
		progress.Counts.OldestEligibleAt = &candidate.At
	}
	return true, nil
}

func (s *Service) persistHistoryCleanupSegmentChunk(ctx context.Context, id string, ordinal int, candidates []historyCleanupCandidateRef, resume historyCleanupRangeCheckpoint) error {
	if len(candidates) == 0 {
		return nil
	}
	chunk := historyCleanupSegmentChunk{SegmentID: id, Ordinal: ordinal, Candidates: candidates, Resume: &resume}
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	key := historyCleanupSegmentChunkKey(id, ordinal)
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	stored, _, readErr := inventory.Read[historyCleanupSegmentChunk](ctx, s.store, key)
	encoded, marshalErr := json.Marshal(stored)
	if readErr != nil || marshalErr != nil || !bytes.Equal(encoded, data) {
		return errors.Join(err, readErr, marshalErr, storage.ErrConditionFailed)
	}
	return nil
}
