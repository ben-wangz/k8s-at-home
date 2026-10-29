package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"sort"
)

type inventoryWorkProgress struct {
	Cursor inventory.Cursor `json:"cursor"`
	Tasks  []inventoryWork  `json:"tasks"`
	Count  int              `json:"count"`
}

func (s *Service) advanceInventoryImageWork(ctx context.Context, p *inventoryImageProgress) (bool, error) {
	image, progress := &p.Image, &p.Work
	prefix := inventory.Prefix + "work/" + image.Identity.ID() + "/"
	if image.Scope == "platform" {
		prefix = inventory.Prefix + "content-work/" + image.ContentID + "/"
	}
	entry, ok, err := progress.Cursor.Peek(ctx, s.store, prefix, 100)
	if err != nil {
		return false, err
	}
	if ok {
		work, _, err := inventory.Read[inventoryWork](ctx, s.store, entry.Key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
		matches := err == nil && (image.Scope == "platform" && work.ContentGeneration == image.ContentGeneration || image.Scope == "reference" && work.Digest == image.ResolvedDigest && work.Generation == image.Generation)
		if matches {
			task, err := s.tasks.Get(ctx, work.Work.ID)
			if err == nil {
				work.Work.Status, work.Work.WaitReason = string(task.Status), task.WaitReason
				work.Work.ErrorClass, work.Work.ErrorMessage = task.ErrorClass, task.ErrorMessage
				work.Work.NextAttemptAt, work.Work.LastProgressAt = optionalTime(task.NextAttemptAt), optionalTime(task.LastProgressAt)
			} else if errors.Is(err, storage.ErrNotFound) {
				latest, _, readErr := inventory.Read[inventoryWork](ctx, s.store, inventory.Prefix+"work/"+work.Identity.ID()+"/"+work.Work.ID+".json")
				if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
					return false, readErr
				}
				if readErr == nil && latest.Generation == work.Generation && latest.Digest == work.Digest && !latest.UpdatedAt.Before(work.UpdatedAt) {
					work.Work = latest.Work
				}
				work.Work.HistoryUnavailable = true
			} else {
				return false, err
			}
			progress.Count++
			progress.Tasks = append(progress.Tasks, work)
			sort.SliceStable(progress.Tasks, func(i, j int) bool {
				left, right := progress.Tasks[i], progress.Tasks[j]
				if inventoryWorkActive(left.Work) != inventoryWorkActive(right.Work) {
					return inventoryWorkActive(left.Work)
				}
				return left.CreatedAt.After(right.CreatedAt)
			})
			progress.Tasks = progress.Tasks[:min(len(progress.Tasks), 32)]
		}
		progress.Cursor.Advance()
	}
	if !progress.Cursor.Done {
		return false, nil
	}
	image.Solidification.TaskCount = progress.Count
	if progress.Count > 32 {
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "task_history_truncated")
	}
	image.Solidification.Tasks = []inventory.Work{}
	for _, work := range progress.Tasks {
		if len(work.Work.ErrorMessage) > 512 {
			work.Work.ErrorMessage = work.Work.ErrorMessage[:512]
		}
		image.Solidification.Tasks = append(image.Solidification.Tasks, work.Work)
		if work.Work.LastProgressAt != nil && (image.Solidification.LastProgressAt == nil || work.Work.LastProgressAt.After(*image.Solidification.LastProgressAt)) {
			image.Solidification.LastProgressAt = work.Work.LastProgressAt
		}
	}
	state, selected := inventory.Execution(image.Solidification.Tasks)
	image.Solidification.ExecutionState = state
	if selected != nil {
		image.Solidification.WaitReason, image.Solidification.NextAttemptAt = selected.WaitReason, selected.NextAttemptAt
	}
	for index := range p.Objects {
		for _, work := range progress.Tasks {
			if !containsInventoryTask(p.Objects[index].TaskIDs, work.Work.ID) {
				p.Objects[index].TaskIDs = append(p.Objects[index].TaskIDs, work.Work.ID)
			}
		}
	}
	return true, nil
}

func containsInventoryTask(ids []string, id string) bool {
	for _, value := range ids {
		if value == id {
			return true
		}
	}
	return false
}
