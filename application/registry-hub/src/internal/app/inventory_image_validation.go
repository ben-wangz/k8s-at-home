package app

import (
	"context"
	"registry-hub/internal/inventory"
	"time"
)

func (s *Service) advanceInventoryAnchor(ctx context.Context, p *inventoryImageProgress) error {
	image := &p.Image
	if p.Anchored || image.Scope != "reference" || image.Reference == image.ResolvedDigest || !p.Closure.Complete || len(p.Objects) == 0 {
		p.Offset, p.Phase = 0, 3
		return nil
	}
	if p.Offset == 0 {
		for _, object := range p.Objects {
			if !object.DistributionVerified {
				p.Phase = 3
				return nil
			}
		}
		published, err := s.inventoryTagPublished(ctx, *image)
		if err != nil {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "local_read_error")
		}
		if err != nil || !published {
			p.Phase = 3
			return nil
		}
	}
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	if gate.Epoch != p.Epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier {
		p.Offset, p.Phase = 0, 3
		return nil
	}
	if p.Offset == len(p.Objects) {
		p.Anchored, image.TagPublished = true, true
		p.Offset, p.Phase = 0, 3
		return nil
	}
	object := &p.Objects[p.Offset]
	generation, err := s.generation(ctx, object.Authority, object.Repository, object.Digest)
	if err != nil {
		return err
	}
	if generation.Tombstone || generation.Generation != object.Generation {
		p.Offset, p.Phase = 0, 3
		return nil
	}
	proof := inventoryProof{SchemaVersion: 1, Generation: object.Generation, Epoch: p.Epoch, State: "anchored", ObservedAt: time.Now().UTC()}
	if err := s.saveInventoryProof(ctx, object.ID, proof); err != nil {
		return err
	}
	object.Anchored = true
	p.Offset++
	return nil
}

func (s *Service) advanceInventoryRecheck(ctx context.Context, p *inventoryImageProgress) error {
	image := &p.Image
	if p.Offset < len(p.Objects) {
		object := p.Objects[p.Offset]
		generation, err := s.generation(ctx, image.Authority, image.Repository, object.Digest)
		if err != nil {
			return err
		}
		p.Invalid = p.Invalid || generation.Tombstone || generation.Generation != object.Generation || object.Digest == image.ResolvedDigest && object.Generation != image.ContentGeneration
		p.Offset++
		return nil
	}
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	p.Invalid = p.Invalid || gate.Epoch != p.Epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier
	if image.Scope == "reference" {
		generation, err := s.generation(ctx, image.Authority, image.Repository, image.Reference)
		if err != nil {
			return err
		}
		p.Invalid = p.Invalid || generation.Tombstone || generation.Generation != image.Generation
		if p.Anchored && image.Reference != image.ResolvedDigest {
			image.TagPublished, err = s.inventoryTagPublished(ctx, *image)
			if err != nil {
				image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "local_read_error")
			}
		}
	}
	if p.Invalid {
		image.Evidence.Freshness = "unknown"
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "generation_or_epoch_changed")
		image.Availability.State = "unknown"
	}
	image.Category = inventory.Classify(*image, p.Anchored)
	p.Offset, p.Phase = 0, 6
	return nil
}

func (s *Service) advanceInventoryRequirements(ctx context.Context, writer *inventory.Writer, p *inventoryImageProgress) error {
	if p.Offset == 0 && p.Image.Scope == "reference" && p.Image.ResolvedDigest != "" {
		if err := writer.RequirePlatform(ctx, p.Image.Identity, p.Content.ObjectRef); err != nil {
			return err
		}
		p.Offset++
		return nil
	}
	start := p.Offset
	if p.Image.Scope == "reference" && p.Image.ResolvedDigest != "" {
		start--
	}
	if start < len(p.Closure.Platforms) {
		if err := writer.RequirePlatform(ctx, p.Image.Identity, p.Closure.RequiredRefs[start]); err != nil {
			return err
		}
		p.Offset++
		return nil
	}
	if err := s.validateCapturedProjection(ctx, &p.Image); err != nil {
		return err
	}
	p.Offset, p.Phase = 0, 7
	return nil
}
