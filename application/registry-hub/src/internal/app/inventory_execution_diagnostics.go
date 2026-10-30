package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type executionProjectionView struct {
	ObservedAt             time.Time                 `json:"observedAt"`
	CapturedFirstStartedAt *time.Time                `json:"capturedFirstStartedAt"`
	KnownFirstStartedAt    *time.Time                `json:"knownFirstStartedAt"`
	ProjectionPending      bool                      `json:"projectionPending"`
	Facts                  []executionProjectionFact `json:"facts"`
	ReadErrors             map[string]string         `json:"readErrors,omitempty"`
	Truncated              bool                      `json:"truncated"`
	Coverage               string                    `json:"coverage"`
	FactsURL               string                    `json:"factsURL"`
}

type executionProjectionFact struct {
	WorkID         string    `json:"workID"`
	TaskID         string    `json:"taskID"`
	RootTaskID     string    `json:"rootTaskID,omitempty"`
	SourceRevision string    `json:"sourceRevision"`
	FirstStartedAt time.Time `json:"firstStartedAt"`
	State          string    `json:"state"`
	StatusURL      string    `json:"statusURL"`
}

func (s *Service) imageExecutionProjection(ctx context.Context, image inventory.Image) executionProjectionView {
	view := executionProjectionView{ObservedAt: time.Now().UTC(), CapturedFirstStartedAt: image.Solidification.FirstStartedAt, Facts: []executionProjectionFact{}, ReadErrors: map[string]string{}, Coverage: "at most four captured root task IDs, for this manifest and content generation; uncaptured tasks, blob demand execution and missing historical facts remain unknown"}
	view.FactsURL = "/api/v1/images/" + image.ID + "/execution-facts?digest=" + image.ResolvedDigest + "&generation=" + strconv.FormatUint(image.ContentGeneration, 10)
	if image.Solidification.FirstStartedAt != nil {
		at := *image.Solidification.FirstStartedAt
		view.KnownFirstStartedAt = &at
	}
	view.Coverage = "up to four independently indexed execution sources plus four legacy captured roots; follow factsURL for the complete indexed scope; legacy/unreconciled blob sources remain unknown"
	indexed := map[string]bool{}
	entries, next, err := storage.Scan(ctx, s.store, executionScopePrefix(image.ID, image.ResolvedDigest, image.ContentGeneration), "", 4)
	if err != nil {
		view.ReadErrors["source_index"] = diagnosticReason(err)
	}
	view.Truncated = next != ""
	for _, entry := range entries {
		link, _, err := inventory.Read[executionSourceLink](ctx, s.store, entry.Key)
		if err != nil {
			view.ReadErrors["source_link"] = diagnosticReason(err)
			continue
		}
		fact, err := s.readExecutionFact(ctx, link)
		if err != nil {
			view.ReadErrors[link.WorkID] = diagnosticReason(err)
			continue
		}
		view.Facts = append(view.Facts, fact)
		indexed[fact.WorkID] = true
		view.ProjectionPending = view.ProjectionPending || fact.State != "done"
		if view.KnownFirstStartedAt == nil || fact.FirstStartedAt.Before(*view.KnownFirstStartedAt) {
			at := fact.FirstStartedAt
			view.KnownFirstStartedAt = &at
		}
	}
	known := 0
	for _, task := range image.Solidification.Tasks {
		if task.Kind != string(tasks.RootKind) {
			continue
		}
		if known == 4 {
			view.Truncated = true
			break
		}
		known++
		id := workqueue.ID("execution", executionSource(task.ID, image.ResolvedDigest, image.ContentGeneration))
		if indexed[id] {
			continue
		}
		job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "execution", id)
		if err != nil {
			view.ReadErrors[task.ID] = diagnosticReason(err)
			continue
		}
		var fact inventoryExecutionFact
		if json.Unmarshal(job.Payload, &fact) != nil || fact.TaskID != task.ID || fact.Digest != image.ResolvedDigest || fact.ContentGeneration != image.ContentGeneration || fact.Identity.Authority != image.Authority || fact.Identity.Repository != image.Repository {
			view.ReadErrors[task.ID] = diagnosticReason(storage.ErrCorruptObject)
			continue
		}
		view.Facts = append(view.Facts, executionProjectionFact{WorkID: id, TaskID: task.ID, SourceRevision: workqueue.SourceRevision(job), FirstStartedAt: job.CreatedAt, State: job.State, StatusURL: "/api/v1/background/execution/" + id})
		view.ProjectionPending = view.ProjectionPending || job.State != "done"
		if view.KnownFirstStartedAt == nil || job.CreatedAt.Before(*view.KnownFirstStartedAt) {
			at := job.CreatedAt
			view.KnownFirstStartedAt = &at
		}
	}
	return view
}
