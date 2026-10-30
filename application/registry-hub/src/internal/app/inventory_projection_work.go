package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"registry-hub/internal/workqueue"
	"strconv"
	"time"
)

type inventoryManifestWork struct {
	Identity          inventory.Identity `json:"identity"`
	Digest            string             `json:"digest"`
	MediaType         string             `json:"mediaType"`
	Generation        uint64             `json:"generation"`
	ContentGeneration uint64             `json:"contentGeneration"`
	KnownNew          bool               `json:"knownNew"`
	Source            string             `json:"source"`
}

type inventoryManifestProgress struct {
	Phase   int                `json:"phase"`
	Content *inventory.Content `json:"content,omitempty"`
	Offset  int                `json:"offset"`
}

func (s *Service) projectManifestStep(ctx context.Context, job *workqueue.Job) (any, bool, error) {
	var source inventoryManifestWork
	var p inventoryManifestProgress
	if json.Unmarshal(job.Payload, &source) != nil || len(job.Progress) > 0 && json.Unmarshal(job.Progress, &p) != nil {
		return p, false, storage.ErrCorruptObject
	}
	contentGeneration, err := s.generation(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest)
	if err != nil {
		return p, false, err
	}
	if contentGeneration.Tombstone || contentGeneration.Generation != source.ContentGeneration {
		job.Superseded = true
		return p, true, nil
	}
	ctx = inventory.WithWriteGuard(ctx, func(ctx context.Context) error {
		current, err := s.generation(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest)
		if err != nil {
			return err
		}
		if current.Tombstone || current.Generation != source.ContentGeneration {
			return storage.ErrConditionFailed
		}
		return nil
	})
	switch p.Phase {
	case 0:
		existing, err := s.inventory.Content(ctx, source.Identity, source.Digest)
		if err == nil && existing.Generation == source.ContentGeneration && existing.ClosureKnown && existing.Source == "manifest" {
			p.Content, p.Phase = &existing, 1
			return p, false, nil
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return p, false, err
		}
		cached, err := s.cache.GetAnyRetained(ctx, source.Identity.Authority, source.Identity.Repository, source.Digest)
		if errors.Is(err, storage.ErrNotFound) {
			if existing.Generation == source.ContentGeneration && existing.ClosureKnown {
				p.Content, p.Phase = &existing, 1
				return p, false, nil
			}
			return p, false, workqueue.ErrSourceMissing
		}
		if err != nil {
			return p, false, err
		}
		defer cached.Body.Close()
		data, err := s.cache.ReadManifestForInventory(ctx, artifact.CachedRepresentation{Authority: source.Identity.Authority, Repository: source.Identity.Repository, Digest: source.Digest, MediaType: cached.MediaType, Size: cached.Size}, s.cfg.Limits.ManifestBytes)
		if err != nil {
			return p, false, err
		}
		links := []inventory.Content{}
		captureCtx := context.WithValue(context.WithValue(ctx, inventoryLinkCapture{}, &links), inventoryObservationKey{}, job.CreatedAt)
		content, err := s.inventoryContent(captureCtx, source.Identity, source.Digest, cached.MediaType, data)
		if err != nil {
			return p, false, err
		}
		p.Content, p.Phase = &content, 1
	case 1:
		if p.Content == nil {
			return p, false, storage.ErrCorruptObject
		}
		refs := append([]inventory.ObjectRef{p.Content.ObjectRef}, p.Content.Children...)
		if p.Offset < len(refs) {
			part := *p.Content
			part.Children = nil
			if p.Offset > 0 {
				part.Children = []inventory.ObjectRef{refs[p.Offset]}
			}
			if err := s.linkInventoryContent(ctx, part); err != nil {
				return p, false, err
			}
			if err := s.inventoryChildDescriptors(ctx, part); err != nil {
				return p, false, err
			}
			p.Offset++
		} else {
			if err := s.markManifestProjected(ctx, source.Identity, source.Digest, source.ContentGeneration); err != nil {
				return p, false, err
			}
			p.Phase, p.Content = 2, nil
		}
	case 2:
		if source.Identity.Reference != "" {
			generation, err := s.generation(ctx, source.Identity.Authority, source.Identity.Repository, source.Identity.Reference)
			if err != nil {
				return p, false, err
			}
			if generation.Tombstone || generation.Generation != source.Generation {
				job.Superseded = true
				return p, true, nil
			}
			_, err = s.inventoryBinding(ctx, source.Identity, source.Digest, source.Source, source.Generation, job.CreatedAt, source.KnownNew)
			if err != nil {
				return p, false, err
			}
		}
		return p, true, s.recordProjectionRevision(ctx, job, source, true)
	default:
		return p, false, storage.ErrCorruptObject
	}
	return p, false, nil
}

func (s *Service) deferManifestProjection(ctx context.Context, source inventoryManifestWork, observed time.Time) error {
	if s.inventory == nil {
		return nil
	}
	key := inventory.ID("manifest-projection", source.Identity.ID(), source.Digest, observed.Format(time.RFC3339Nano), strconv.FormatUint(source.Generation, 10), strconv.FormatUint(source.ContentGeneration, 10), strconv.FormatBool(source.KnownNew), source.Source)
	return s.enqueueProjection(ctx, "inventory", key, source, observed)
}

func (s *Service) deferDiscoveredManifestProjection(ctx context.Context, task *tasks.Task, digest, mediaType string) {
	if s.inventory == nil {
		return
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	inspection, err := s.cache.Inspect(bounded, task.Authority, task.Repository, digest, mediaType)
	if err != nil {
		s.backgroundEvent("inventory", "enqueue_failed", err)
		return
	}
	if !inspection.Present || inspection.VerifiedAt.IsZero() {
		return
	}
	generation, err := s.generation(bounded, task.Authority, task.Repository, digest)
	if err != nil {
		s.backgroundEvent("inventory", "enqueue_failed", err)
		return
	}
	if generation.Tombstone || inspection.VerifiedAt.Before(generation.UpdatedAt) {
		return
	}
	_ = s.deferManifestProjection(bounded, inventoryManifestWork{Identity: inventoryIdentity(task.Authority, task.Repository, digest), Digest: digest, MediaType: mediaType, Generation: generation.Generation, ContentGeneration: generation.Generation, KnownNew: inspection.InventoryTracked, Source: "cache"}, inspection.VerifiedAt)
}
