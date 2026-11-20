package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

var errImmutableBatchMismatch = errors.New("immutable_batch_mismatch")

type storageUsagePending struct {
	Version        int                 `json:"version"`
	BatchID        string              `json:"batchID"`
	SnapshotID     string              `json:"snapshotID"`
	Run            storageUsageRun     `json:"run"`
	StartCursor    inventory.Cursor    `json:"startCursor"`
	NextCursor     inventory.Cursor    `json:"nextCursor"`
	NextPhase      int                 `json:"nextPhase"`
	PayloadKey     string              `json:"payloadKey"`
	PayloadSHA256  string              `json:"payloadSHA256"`
	PageKey        string              `json:"pageKey"`
	PageSHA256     string              `json:"pageSHA256"`
	ManifestKey    string              `json:"manifestKey"`
	Manifest       storageUsageRunMeta `json:"manifest"`
	ManifestSHA256 string              `json:"manifestSHA256"`
	PreparedAt     time.Time           `json:"preparedAt"`
}

func storageUsageDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneStorageUsageCursor(cursor inventory.Cursor) inventory.Cursor {
	cursor.Entries = append([]storage.Entry(nil), cursor.Entries...)
	return cursor
}

func cloneStorageUsage(rows map[string]storageUsage) map[string]storageUsage {
	copy := make(map[string]storageUsage, len(rows))
	for key, row := range rows {
		copy[key] = row
	}
	return copy
}

func (s *Service) prepareStorageUsagePending(ctx context.Context, before, after storageCollection) (*storageUsagePending, error) {
	rows := make([]storageUsage, 0, len(after.Usage))
	for _, usage := range after.Usage {
		rows = append(rows, usage)
	}
	sortStorageUsage(rows, false)
	payload, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	payloadDigest := storageUsageDigest(payload)
	run := storageUsageRun{Stage: "reduce", Index: before.UsageRuns}
	batchID := inventory.ID("storage-usage-batch", before.Snapshot.SnapshotID, fmt.Sprint(run.Index), payloadDigest)
	payloadKey := fmt.Sprintf("%ssnapshots/%s/usage/pending/%s.json", storageInventoryPrefix, before.Snapshot.SnapshotID, batchID)
	if err := s.writeStorageUsageImmutable(ctx, payloadKey, payload, before.Snapshot.SnapshotID, batchID, payloadDigest); err != nil {
		return nil, err
	}
	manifest := storageUsageRunMeta{Pages: 1, Rows: len(rows)}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	pageKey := usageRunPage(before.Snapshot.SnapshotID, run, 0)
	manifestKey := usageRunPrefix(before.Snapshot.SnapshotID, run) + "manifest.json"
	return &storageUsagePending{
		Version: 1, BatchID: batchID, SnapshotID: before.Snapshot.SnapshotID, Run: run,
		StartCursor: cloneStorageUsageCursor(before.AttributionUsageCursor),
		NextCursor:  cloneStorageUsageCursor(after.AttributionUsageCursor), NextPhase: after.AttributionPhase,
		PayloadKey: payloadKey, PayloadSHA256: payloadDigest, PageKey: pageKey, PageSHA256: payloadDigest,
		ManifestKey: manifestKey, Manifest: manifest, ManifestSHA256: storageUsageDigest(manifestData),
		PreparedAt: time.Now().UTC(),
	}, nil
}

func (s *Service) writeStorageUsageImmutable(ctx context.Context, key string, expected []byte, buildID, batchID, expectedDigest string) error {
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err := s.store.Put(ctx, key, bytes.NewReader(expected), int64(len(expected)), storage.PutOptions{IfAbsent: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrUnknownCommit) && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	actual, _, readErr := inventory.Read[json.RawMessage](ctx, s.store, key)
	if readErr != nil {
		return errors.Join(err, readErr)
	}
	if bytes.Equal(actual, expected) {
		return nil
	}
	actualDigest := storageUsageDigest(actual)
	return errors.Join(errImmutableBatchMismatch, storage.ErrConditionFailed, fmt.Errorf(
		"build=%s batch=%s key=%s expected_sha256=%s actual_sha256=%s",
		buildID, batchID, key, expectedDigest, actualDigest,
	))
}

func (s *Service) loadStorageUsagePendingRows(ctx context.Context, pending storageUsagePending) ([]storageUsage, error) {
	raw, _, err := inventory.Read[json.RawMessage](ctx, s.store, pending.PayloadKey)
	if err != nil {
		return nil, err
	}
	if storageUsageDigest(raw) != pending.PayloadSHA256 || storageUsageDigest(raw) != pending.PageSHA256 {
		return nil, storage.ErrCorruptObject
	}
	var rows []storageUsage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, errors.Join(storage.ErrCorruptObject, err)
	}
	if len(rows) != pending.Manifest.Rows || pending.Manifest.Pages != 1 {
		return nil, storage.ErrCorruptObject
	}
	return rows, nil
}

func (s *Service) publishStorageUsagePending(ctx context.Context, state *storageCollection) error {
	pending := state.UsagePending
	if pending == nil {
		return nil
	}
	if pending.Version != 1 || pending.SnapshotID != state.Snapshot.SnapshotID ||
		pending.Run != (storageUsageRun{Stage: "reduce", Index: state.UsageRuns}) ||
		!reflect.DeepEqual(pending.StartCursor, state.AttributionUsageCursor) {
		return storage.ErrCorruptObject
	}
	rows, err := s.loadStorageUsagePendingRows(ctx, *pending)
	if err != nil {
		return err
	}
	page, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	manifest, err := json.Marshal(pending.Manifest)
	if err != nil {
		return err
	}
	if storageUsageDigest(page) != pending.PageSHA256 || storageUsageDigest(manifest) != pending.ManifestSHA256 ||
		pending.PageKey != usageRunPage(pending.SnapshotID, pending.Run, 0) ||
		pending.ManifestKey != usageRunPrefix(pending.SnapshotID, pending.Run)+"manifest.json" {
		return storage.ErrCorruptObject
	}
	if err := s.writeStorageUsageImmutable(ctx, pending.PageKey, page, pending.SnapshotID, pending.BatchID, pending.PageSHA256); err != nil {
		return err
	}
	if err := s.writeStorageUsageImmutable(ctx, pending.ManifestKey, manifest, pending.SnapshotID, pending.BatchID, pending.ManifestSHA256); err != nil {
		return err
	}
	state.AttributionUsageCursor = cloneStorageUsageCursor(pending.NextCursor)
	state.AttributionPhase = pending.NextPhase
	state.UsageRuns++
	state.Usage = map[string]storageUsage{}
	state.UsagePending = nil
	return nil
}
