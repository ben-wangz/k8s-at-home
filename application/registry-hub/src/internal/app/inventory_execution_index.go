package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type executionSourceLink struct {
	ImageID        string    `json:"imageID"`
	Digest         string    `json:"digest"`
	Generation     uint64    `json:"generation"`
	WorkID         string    `json:"workID"`
	TaskID         string    `json:"taskID"`
	ObservedAt     time.Time `json:"observedAt"`
	SourceRevision string    `json:"sourceRevision"`
	RootTaskID     string    `json:"rootTaskID,omitempty"`
}

type executionFactPage struct {
	diagnosticPage[executionProjectionFact]
	Coverage string `json:"coverage"`
}

type compactExecutionFact struct {
	SchemaVersion  int                    `json:"schemaVersion"`
	WorkID         string                 `json:"workID"`
	SourceRevision string                 `json:"sourceRevision"`
	Fact           inventoryExecutionFact `json:"fact"`
	FirstStartedAt time.Time              `json:"firstStartedAt"`
	State          string                 `json:"state"`
	Superseded     bool                   `json:"superseded,omitempty"`
	CompletedAt    time.Time              `json:"completedAt"`
	FinalSummary   json.RawMessage        `json:"finalSummary,omitempty"`
}

const compactExecutionFactPrefix = inventory.Prefix + "execution-facts/"

func compactExecutionFactKey(workID string) string {
	return compactExecutionFactPrefix + workID + ".json"
}

func executionIndexPrefix(imageID string) string {
	return inventory.Prefix + "execution-sources/" + imageID + "/"
}

func executionScopePrefix(imageID, digest string, generation uint64) string {
	return executionIndexPrefix(imageID) + inventory.ID(digest, strconv.FormatUint(generation, 10)) + "/"
}

func executionSourceTargets(fact inventoryExecutionFact) []executionSourceLink {
	return []executionSourceLink{
		{ImageID: fact.Identity.ID(), Digest: fact.RootDigest, Generation: fact.RootContentGeneration},
		{ImageID: inventory.ID("platform", fact.Identity.Authority, fact.Identity.Repository, fact.RootDigest), Digest: fact.RootDigest, Generation: fact.RootContentGeneration},
		{ImageID: inventory.ID("platform", fact.Identity.Authority, fact.Identity.Repository, fact.Digest), Digest: fact.Digest, Generation: fact.ContentGeneration},
	}
}

func (s *Service) indexExecutionSource(ctx context.Context, job workqueue.Job, fact inventoryExecutionFact) error {
	if fact.Kind == tasks.BlobKind {
		return nil
	}
	targets := executionSourceTargets(fact)
	for _, target := range targets {
		if target.Digest == "" {
			continue
		}
		target.WorkID, target.TaskID, target.ObservedAt = job.ID, fact.TaskID, job.CreatedAt
		if err := s.saveExecutionLink(ctx, job, target); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) verifyExecutionSourceLinks(ctx context.Context, job workqueue.Job, fact inventoryExecutionFact) (bool, error) {
	if fact.Kind == tasks.BlobKind {
		return s.verifyBlobExecutionSourceLinks(ctx, job, fact)
	}
	seen := map[string]struct{}{}
	for _, target := range executionSourceTargets(fact) {
		if target.Digest == "" {
			continue
		}
		target.WorkID, target.TaskID, target.ObservedAt, target.SourceRevision = job.ID, fact.TaskID, job.CreatedAt, workqueue.SourceRevision(job)
		key := executionScopePrefix(target.ImageID, target.Digest, target.Generation) + job.ID + ".json"
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		stored, _, err := inventory.Read[executionSourceLink](ctx, s.store, key)
		if errors.Is(err, storage.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if stored != target {
			return false, storage.ErrCorruptObject
		}
	}
	return len(seen) > 0, nil
}

func (s *Service) projectCompactExecutionFact(ctx context.Context, entry storage.Entry, job workqueue.Job, fact inventoryExecutionFact) error {
	if job.Kind != "execution" || job.State != "done" || job.ID == "" || fact.TaskID == "" {
		return storage.ErrCorruptObject
	}
	completedAt := job.CompletedAt
	if completedAt.IsZero() {
		if entry.ModTime == 0 {
			return storage.ErrCorruptObject
		}
		completedAt = time.Unix(0, entry.ModTime).UTC()
	}
	compact := compactExecutionFact{
		SchemaVersion: 1, WorkID: job.ID, SourceRevision: workqueue.SourceRevision(job), Fact: fact,
		FirstStartedAt: job.CreatedAt, State: job.State, Superseded: job.Superseded, CompletedAt: completedAt.UTC(),
		FinalSummary: append(json.RawMessage(nil), job.FinalSummary...),
	}
	return inventory.Update(ctx, s.store, compactExecutionFactKey(job.ID), func(current *compactExecutionFact) error {
		if current.SchemaVersion != 0 {
			if current.SchemaVersion != compact.SchemaVersion || current.WorkID != compact.WorkID || current.SourceRevision != compact.SourceRevision {
				return storage.ErrCorruptObject
			}
			oldFact, _ := json.Marshal(current.Fact)
			newFact, _ := json.Marshal(compact.Fact)
			if !bytes.Equal(oldFact, newFact) {
				return storage.ErrCorruptObject
			}
			if current.CompletedAt.After(compact.CompletedAt) {
				return nil
			}
		}
		*current = compact
		return nil
	})
}

func (s *Service) saveExecutionLink(ctx context.Context, job workqueue.Job, target executionSourceLink) error {
	target.SourceRevision = workqueue.SourceRevision(job)
	key := executionScopePrefix(target.ImageID, target.Digest, target.Generation) + job.ID + ".json"
	return inventory.Update(ctx, s.store, key, func(value *executionSourceLink) error { *value = target; return nil })
}

func (s *Service) readExecutionFact(ctx context.Context, link executionSourceLink) (executionProjectionFact, error) {
	projection, fact, err := s.readExecutionProjection(ctx, link.WorkID)
	if err != nil {
		return executionProjectionFact{}, err
	}
	if fact.TaskID != link.TaskID || link.SourceRevision != projection.SourceRevision || link.RootTaskID == "" &&
		(link.Digest != fact.RootDigest && link.Digest != fact.Digest ||
			link.Digest == fact.RootDigest && link.Generation != fact.RootContentGeneration ||
			link.Digest == fact.Digest && link.Generation != fact.ContentGeneration) {
		return executionProjectionFact{}, storage.ErrCorruptObject
	}
	projection.RootTaskID = link.RootTaskID
	return projection, nil
}

func (s *Service) readExecutionProjection(ctx context.Context, workID string) (executionProjectionFact, inventoryExecutionFact, error) {
	compact, _, compactErr := inventory.Read[compactExecutionFact](ctx, s.store, compactExecutionFactKey(workID))
	if compactErr != nil && !errors.Is(compactErr, storage.ErrNotFound) {
		return executionProjectionFact{}, inventoryExecutionFact{}, compactErr
	}
	job, jobErr := (workqueue.Queue{Store: s.store}).Read(ctx, "execution", workID)
	if jobErr != nil && !errors.Is(jobErr, storage.ErrNotFound) {
		return executionProjectionFact{}, inventoryExecutionFact{}, jobErr
	}
	if errors.Is(jobErr, storage.ErrNotFound) && errors.Is(compactErr, storage.ErrNotFound) {
		return executionProjectionFact{}, inventoryExecutionFact{}, storage.ErrNotFound
	}
	var fact inventoryExecutionFact
	var sourceRevision string
	var firstStartedAt time.Time
	var state string
	if compactErr == nil {
		if compact.SchemaVersion != 1 || compact.WorkID != workID || compact.State != "done" || compact.CompletedAt.IsZero() || compact.FirstStartedAt.IsZero() {
			return executionProjectionFact{}, inventoryExecutionFact{}, storage.ErrCorruptObject
		}
		fact, sourceRevision, firstStartedAt, state = compact.Fact, compact.SourceRevision, compact.FirstStartedAt, compact.State
	}
	if jobErr == nil {
		var liveFact inventoryExecutionFact
		if job.ID != workID || job.Kind != "execution" || json.Unmarshal(job.Payload, &liveFact) != nil {
			return executionProjectionFact{}, inventoryExecutionFact{}, storage.ErrCorruptObject
		}
		liveRevision := workqueue.SourceRevision(job)
		if compactErr == nil && (sourceRevision != liveRevision || !sameExecutionFact(fact, liveFact)) {
			return executionProjectionFact{}, inventoryExecutionFact{}, storage.ErrCorruptObject
		}
		fact, sourceRevision, firstStartedAt, state = liveFact, liveRevision, job.CreatedAt, job.State
	}
	if fact.TaskID == "" || sourceRevision == "" || firstStartedAt.IsZero() || state == "" {
		return executionProjectionFact{}, inventoryExecutionFact{}, storage.ErrCorruptObject
	}
	return executionProjectionFact{
		WorkID: workID, TaskID: fact.TaskID, SourceRevision: sourceRevision, FirstStartedAt: firstStartedAt,
		State: state, StatusURL: "/api/v1/background/execution/" + workID,
	}, fact, nil
}

func sameExecutionFact(left, right inventoryExecutionFact) bool {
	leftData, leftErr := json.Marshal(left)
	rightData, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftData, rightData)
}
