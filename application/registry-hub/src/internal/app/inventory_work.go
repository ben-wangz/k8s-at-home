package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"sort"
)

func (s *Service) inventoryImageWork(ctx context.Context, image *inventory.Image, objects []inventory.Object) error {
	prefix := inventory.Prefix + "work/" + image.Identity.ID() + "/"
	if image.Scope == "platform" {
		prefix = inventory.Prefix + "content-work/" + image.ContentID + "/"
	}
	works := []inventoryWork{}
	token := ""
	for {
		entries, next, err := storage.Scan(ctx, s.store, prefix, token, 100)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			work, _, err := inventory.Read[inventoryWork](ctx, s.store, entry.Key)
			if err != nil {
				return err
			}
			if image.Scope == "reference" && (work.Digest != image.ResolvedDigest || work.Generation != image.Generation) {
				continue
			}
			if image.Scope == "platform" && work.ContentGeneration != image.ContentGeneration {
				continue
			}
			task, err := s.tasks.Get(ctx, work.Work.ID)
			if err == nil {
				work.Work.Status = string(task.Status)
				work.Work.WaitReason = task.WaitReason
				work.Work.ErrorClass = task.ErrorClass
				work.Work.ErrorMessage = task.ErrorMessage
				work.Work.NextAttemptAt = optionalTime(task.NextAttemptAt)
				work.Work.LastProgressAt = optionalTime(task.LastProgressAt)
			} else if errors.Is(err, storage.ErrNotFound) {
				work.Work.HistoryUnavailable = true
			} else {
				return err
			}
			works = append(works, work)
		}
		if next == "" {
			break
		}
		token = next
	}
	sort.Slice(works, func(i, j int) bool { return works[i].CreatedAt.After(works[j].CreatedAt) })
	sort.SliceStable(works, func(i, j int) bool { return inventoryWorkActive(works[i].Work) && !inventoryWorkActive(works[j].Work) })
	image.Solidification.TaskCount = len(works)
	if len(works) > 32 {
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "task_history_truncated")
		works = works[:32]
	}
	image.Solidification.Tasks = []inventory.Work{}
	for _, work := range works {
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
		image.Solidification.WaitReason = selected.WaitReason
		image.Solidification.NextAttemptAt = selected.NextAttemptAt
	}
	for index := range objects {
		for _, work := range works {
			found := false
			for _, id := range objects[index].TaskIDs {
				if id == work.Work.ID {
					found = true
					break
				}
			}
			if !found {
				objects[index].TaskIDs = append(objects[index].TaskIDs, work.Work.ID)
			}
		}
	}
	return nil
}

func inventoryWorkActive(work inventory.Work) bool {
	state, _ := inventory.Execution([]inventory.Work{work})
	return state != "idle" && state != "not_scheduled"
}

func (s *Service) inventoryTagPublished(ctx context.Context, image inventory.Image) (bool, error) {
	internal, err := inventoryInternalName(image.Identity)
	if err != nil {
		return false, err
	}
	response, err := s.registry.Do(ctx, http.MethodHead, "/v2/"+internal+"/manifests/"+url.PathEscape(image.Reference), nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodHead, route.EndpointManifest) {
		return false, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, storage.ErrUnavailable
	}
	return response.Header.Get("Docker-Content-Digest") == image.ResolvedDigest && response.ContentLength >= 0, nil
}
