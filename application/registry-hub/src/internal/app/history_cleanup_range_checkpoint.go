package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func historyCleanupRangeCheckpointFromProgress(progress historyCleanupRangeProgress) historyCleanupRangeCheckpoint {
	return historyCleanupRangeCheckpoint{
		PrefixIndex: progress.PrefixIndex, Cursor: progress.Cursor, Scanned: progress.Scanned, Counts: progress.Counts,
		ChunkIndex: progress.ChunkIndex, SegmentID: progress.SegmentID, SegmentItems: progress.SegmentItems,
		SegmentObjects: progress.SegmentObjects, SegmentBytes: progress.SegmentBytes, SnapshotID: progress.SnapshotID,
		Snapshot: progress.Snapshot, FinishedAt: progress.FinishedAt,
	}
}

func (checkpoint historyCleanupRangeCheckpoint) progress() historyCleanupRangeProgress {
	return historyCleanupRangeProgress{
		PrefixIndex: checkpoint.PrefixIndex, Cursor: checkpoint.Cursor, Scanned: checkpoint.Scanned, Counts: checkpoint.Counts,
		ChunkIndex: checkpoint.ChunkIndex, SegmentID: checkpoint.SegmentID, SegmentItems: checkpoint.SegmentItems,
		SegmentObjects: checkpoint.SegmentObjects, SegmentBytes: checkpoint.SegmentBytes, SnapshotID: checkpoint.SnapshotID,
		Snapshot: checkpoint.Snapshot, FinishedAt: checkpoint.FinishedAt,
	}
}

func (s *Service) readHistoryCleanupRangeOrphanChunk(ctx context.Context, segmentID string, ordinal int) (historyCleanupRangeProgress, bool, error) {
	if segmentID == "" || ordinal < 0 {
		return historyCleanupRangeProgress{}, false, storage.ErrCorruptObject
	}
	chunk, _, err := inventory.Read[historyCleanupSegmentChunk](ctx, s.store, historyCleanupSegmentChunkKey(segmentID, ordinal))
	if errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeProgress{}, false, nil
	}
	if err != nil {
		return historyCleanupRangeProgress{}, false, err
	}
	if chunk.SegmentID != segmentID || chunk.Ordinal != ordinal || len(chunk.Candidates) == 0 || chunk.Resume == nil ||
		chunk.Resume.SegmentID != segmentID || chunk.Resume.ChunkIndex != ordinal+1 || chunk.Resume.ChunkIndex < 0 {
		return historyCleanupRangeProgress{}, false, storage.ErrCorruptObject
	}
	return chunk.Resume.progress(), true, nil
}

func (s *Service) putHistoryCleanupContinuation(ctx context.Context, id string, continuation historyCleanupRangeContinuation) error {
	data, err := json.Marshal(continuation)
	if err != nil {
		return err
	}
	key := historyCleanupContinuationKey(id)
	_, err = s.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	stored, _, readErr := inventory.Read[historyCleanupRangeContinuation](ctx, s.store, key)
	encoded, marshalErr := json.Marshal(stored)
	if readErr != nil || marshalErr != nil || !bytes.Equal(encoded, data) {
		return errors.Join(err, readErr, marshalErr, storage.ErrConditionFailed)
	}
	return nil
}

func (s *Service) readHistoryCleanupRangeProgress(ctx context.Context, id string, fallback json.RawMessage) (historyCleanupRangeProgress, error) {
	record, _, err := inventory.Read[historyCleanupRangeProgressRecord](ctx, s.store, historyCleanupRangeProgressKey(id))
	if err == nil {
		if record.SchemaVersion != 1 || record.PreviewID != id {
			return historyCleanupRangeProgress{}, storage.ErrCorruptObject
		}
		return record.Progress, nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return historyCleanupRangeProgress{}, err
	}
	var progress historyCleanupRangeProgress
	if len(fallback) > 0 && json.Unmarshal(fallback, &progress) != nil {
		return historyCleanupRangeProgress{}, storage.ErrCorruptObject
	}
	return progress, nil
}

func (s *Service) saveHistoryCleanupRangeProgress(ctx context.Context, preview historyCleanupRangePreview, progress historyCleanupRangeProgress) error {
	if err := s.saveHistoryCleanupRangeCheckpoint(ctx, preview.ID, progress); err != nil {
		return err
	}
	return inventory.Update(ctx, s.store, historyCleanupRangePreviewKey(preview.ID), func(current *historyCleanupRangePreview) error {
		if current.SchemaVersion != 1 || current.Principal != preview.Principal || current.PolicyVersion != preview.PolicyVersion {
			return storage.ErrConditionFailed
		}
		current.Status, current.Counts = preview.Status, preview.Counts
		current.SegmentID, current.SegmentItems, current.SegmentBytes = preview.SegmentID, preview.SegmentItems, preview.SegmentBytes
		current.NextCursor, current.BackfillPending, current.CompletedAt = preview.NextCursor, preview.BackfillPending, preview.CompletedAt
		current.ExpiresAt = preview.ExpiresAt
		return nil
	})
}

func (s *Service) saveHistoryCleanupRangeCheckpoint(ctx context.Context, id string, progress historyCleanupRangeProgress) error {
	return inventory.Update(ctx, s.store, historyCleanupRangeProgressKey(id), func(record *historyCleanupRangeProgressRecord) error {
		if record.SchemaVersion != 1 || record.PreviewID != id {
			return storage.ErrCorruptObject
		}
		record.Progress = progress
		return nil
	})
}
