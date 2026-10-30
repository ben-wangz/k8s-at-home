package app

import (
	"context"
	"time"
)

func (s *Service) transferStorageSnapshot(ctx context.Context) *storageSnapshot {
	snapshot, err := s.storageSnapshot(ctx, "")
	if err != nil {
		return nil
	}
	return &snapshot
}

func attachTransferStorage(view *transferView, snapshot *storageSnapshot) {
	view.TemporaryBytes, view.TemporaryObservedAt = nil, time.Time{}
	view.TemporaryReason = "physical_inventory_not_observed_or_expired"
	if snapshot == nil {
		return
	}
	view.StorageSnapshotID = snapshot.SnapshotID
	view.TemporaryObservedAt = snapshot.CompletedAt
	if bytes, ok := snapshot.TransferTemporaryBytes[view.ID]; ok {
		view.TemporaryBytes, view.TemporaryReason = &bytes, "observed_associated_objects; unassociated_objects_may_exist"
	} else {
		view.TemporaryReason = "no_associated_objects_observed; absence_is_not_proof_of_zero_residual_bytes"
	}
}
