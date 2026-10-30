package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"strings"
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

func executionIndexPrefix(imageID string) string {
	return inventory.Prefix + "execution-sources/" + imageID + "/"
}

func executionScopePrefix(imageID, digest string, generation uint64) string {
	return executionIndexPrefix(imageID) + inventory.ID(digest, strconv.FormatUint(generation, 10)) + "/"
}

func (s *Service) indexExecutionSource(ctx context.Context, job workqueue.Job, fact inventoryExecutionFact) error {
	if fact.Kind == tasks.BlobKind {
		return nil
	}
	targets := []executionSourceLink{
		{ImageID: fact.Identity.ID(), Digest: fact.RootDigest, Generation: fact.RootContentGeneration},
		{ImageID: inventory.ID("platform", fact.Identity.Authority, fact.Identity.Repository, fact.RootDigest), Digest: fact.RootDigest, Generation: fact.RootContentGeneration},
		{ImageID: inventory.ID("platform", fact.Identity.Authority, fact.Identity.Repository, fact.Digest), Digest: fact.Digest, Generation: fact.ContentGeneration},
	}
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

func (s *Service) saveExecutionLink(ctx context.Context, job workqueue.Job, target executionSourceLink) error {
	target.SourceRevision = workqueue.SourceRevision(job)
	key := executionScopePrefix(target.ImageID, target.Digest, target.Generation) + job.ID + ".json"
	return inventory.Update(ctx, s.store, key, func(value *executionSourceLink) error { *value = target; return nil })
}

func (s *Service) readExecutionFact(ctx context.Context, link executionSourceLink) (executionProjectionFact, error) {
	job, err := (workqueue.Queue{Store: s.store}).Read(ctx, "execution", link.WorkID)
	if err != nil {
		return executionProjectionFact{}, err
	}
	var fact inventoryExecutionFact
	if json.Unmarshal(job.Payload, &fact) != nil || fact.TaskID != link.TaskID || link.SourceRevision != workqueue.SourceRevision(job) {
		return executionProjectionFact{}, storage.ErrCorruptObject
	}
	return executionProjectionFact{WorkID: job.ID, TaskID: fact.TaskID, RootTaskID: link.RootTaskID, SourceRevision: workqueue.SourceRevision(job), FirstStartedAt: link.ObservedAt, State: job.State, StatusURL: "/api/v1/background/execution/" + job.ID}, nil
}

func (s *Service) serveExecutionFacts(writer http.ResponseWriter, request *http.Request, ctx context.Context) {
	id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/v1/images/"), "/execution-facts")
	values, cursor, limit, err := s.diagnosticQuery(request, "digest", "generation")
	if err != nil || !inventory.ValidID(id) {
		writeInventoryError(writer, inventory.ErrInvalid)
		return
	}
	if raw := values.Get("generation"); raw != "" {
		if _, parseErr := strconv.ParseUint(raw, 10, 64); parseErr != nil {
			writeInventoryError(writer, inventory.ErrInvalid)
			return
		}
	}
	prefix := executionIndexPrefix(id)
	if values.Get("digest") != "" && values.Get("generation") != "" {
		generation, _ := strconv.ParseUint(values.Get("generation"), 10, 64)
		prefix = executionScopePrefix(id, values.Get("digest"), generation)
	}
	entries, next, err := storage.Scan(ctx, s.store, prefix, cursor.Storage, limit)
	if err != nil {
		writeInventoryError(writer, err)
		return
	}
	page := executionFactPage{diagnosticPage: diagnosticPage[executionProjectionFact]{Items: []executionProjectionFact{}, ObservedAt: time.Now().UTC(), Consistency: "durable execution source links; live job/receipt state", Scanned: len(entries)}, Coverage: "root sources indexed before execution; blob sources indexed from durable demands and generation-qualified graph nodes; legacy and unreconciled associations remain unknown; absence does not prove never started"}
	for _, entry := range entries {
		link, _, err := inventory.Read[executionSourceLink](ctx, s.store, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		if link.ImageID != id {
			writeInventoryError(writer, storage.ErrCorruptObject)
			return
		}
		if values.Get("digest") != "" && values.Get("digest") != link.Digest || values.Get("generation") != "" && values.Get("generation") != strconv.FormatUint(link.Generation, 10) {
			continue
		}
		fact, err := s.readExecutionFact(ctx, link)
		if err != nil {
			writeInventoryError(writer, err)
			return
		}
		page.Items = append(page.Items, fact)
	}
	if next != "" {
		cursor.Storage = next
		page.NextToken = s.diagnosticToken(cursor)
	}
	writeJSON(writer, http.StatusOK, page)
}
