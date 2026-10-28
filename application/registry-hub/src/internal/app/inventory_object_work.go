package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *Service) inventoryObjectWork(ctx context.Context, object *inventory.Object) error {
	if object.Role == "manifest" || object.Authority == "" {
		return nil
	}
	intent, err := s.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: object.Authority, Repository: object.Repository, Digest: object.Digest})
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.Generation != object.Generation {
		return nil
	}
	object.IntentID, object.IntentState = intent.ID, string(intent.State)
	object.TaskIDs = append(object.TaskIDs, intent.ID)
	demands, next, err := s.records.ListBlobDemandRecords(ctx, intent.ID, "", 100)
	if err != nil {
		return err
	}
	if next != "" {
		object.Reasons = appendReason(object.Reasons, "task_associations_truncated")
	}
	seen := map[string]bool{intent.ID: true}
	for _, demand := range demands {
		if demand.RootTaskID != "" && !seen[demand.RootTaskID] {
			object.TaskIDs = append(object.TaskIDs, demand.RootTaskID)
			seen[demand.RootTaskID] = true
		}
	}
	return nil
}
