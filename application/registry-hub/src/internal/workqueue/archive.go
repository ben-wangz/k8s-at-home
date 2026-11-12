package workqueue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

func (q Queue) Archive(ctx context.Context, job Job) error {
	if job.State != "done" {
		return storage.ErrConditionFailed
	}
	active, version, err := inventory.Read[Job](ctx, q.Store, Key(job.Kind, job.ID))
	if err == nil && (version != job.Version || active.State != "done" || active.Fence != job.Fence || !bytes.Equal(active.Payload, job.Payload)) {
		return storage.ErrConditionFailed
	}
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	hasActive := err == nil
	if hasActive {
		active.Version = version
		job = active
	} else {
		// Only authoritative active progress may upgrade a legacy receipt.
		job.Progress = nil
	}
	if err := q.summarize(&job); err != nil {
		return err
	}
	job.Progress = nil
	data, err := json.Marshal(job)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = q.Store.Put(ctx, ReceiptKey(job.Kind, job.ID), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		err = q.preserveReceiptSummary(ctx, job, hasActive)
	}
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	err = q.Store.DeleteIfMatch(ctx, Key(job.Kind, job.ID), job.Version)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	return err
}

func (q Queue) preserveReceiptSummary(ctx context.Context, job Job, hasActive bool) error {
	stored, version, err := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID))
	if err != nil {
		return err
	}
	if stored.ID != job.ID || stored.Kind != job.Kind || stored.State != "done" || !bytes.Equal(stored.Payload, job.Payload) {
		return storage.ErrCorruptObject
	}
	if !hasActive || validFinalSummary(stored.FinalSummary) || !validFinalSummary(job.FinalSummary) {
		return nil
	}
	if stored.Fence != job.Fence {
		return storage.ErrConditionFailed
	}
	stored.FinalSummary = job.FinalSummary
	encoded, err := json.Marshal(stored)
	if err != nil {
		return err
	}
	if err := inventory.ValidateWriteGuard(ctx); err != nil {
		return err
	}
	_, err = q.Store.Put(ctx, ReceiptKey(job.Kind, job.ID), bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{IfMatch: version})
	if errors.Is(err, storage.ErrUnknownCommit) {
		actual, _, readErr := inventory.Read[Job](ctx, q.Store, ReceiptKey(job.Kind, job.ID))
		if readErr == nil && bytes.Equal(actual.FinalSummary, job.FinalSummary) && bytes.Equal(actual.Payload, job.Payload) {
			return nil
		}
		return errors.Join(err, readErr)
	}
	return err
}
