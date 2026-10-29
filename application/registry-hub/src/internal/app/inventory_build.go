package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

func (s *Service) buildInventoryContent(ctx context.Context, writer *inventory.Writer, key string) error {
	content, _, err := inventory.Read[inventory.Content](ctx, s.store, key)
	if err != nil {
		return err
	}
	return s.buildInventoryPlatform(ctx, writer, content)
}

func (s *Service) buildInventoryPlatform(ctx context.Context, writer *inventory.Writer, content inventory.Content) error {
	identity := content.Identity
	identity.Reference = content.Digest
	image := inventory.Image{ID: inventory.ID("platform", identity.Authority, identity.Repository, content.Digest), ContentID: identity.ContentID(content.Digest), Scope: "platform", Identity: identity, ResolvedDigest: content.Digest, ContentGeneration: content.Generation}
	return s.buildInventoryImage(ctx, writer, image, content)
}

func (s *Service) buildInventoryReference(ctx context.Context, writer *inventory.Writer, key string) error {
	binding, _, err := inventory.Read[inventory.Binding](ctx, s.store, key)
	if err != nil {
		return err
	}
	content, err := s.inventory.Content(ctx, binding.Identity, binding.Digest)
	if errors.Is(err, storage.ErrNotFound) {
		content = inventory.Content{Identity: binding.Identity, ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: binding.Digest}, Generation: binding.ContentGeneration}
	} else if err != nil {
		return err
	}
	image := inventory.Image{ID: binding.Identity.ID(), ContentID: binding.ContentID(binding.Digest), Scope: "reference", Identity: binding.Identity, ResolvedDigest: binding.Digest, BindingRevision: binding.Revision, BindingSource: binding.Source, BindingConfirmedAt: binding.ConfirmedAt, Generation: binding.Generation, ContentGeneration: binding.ContentGeneration}
	return s.buildInventoryImage(ctx, writer, image, content)
}

func (s *Service) buildInventoryImage(ctx context.Context, writer *inventory.Writer, image inventory.Image, content inventory.Content) error {
	ready, err := writer.HasImage(ctx, image.ID)
	if err != nil || ready {
		return err
	}
	image.MediaType, image.ArtifactType, image.Platform = content.MediaType, content.ArtifactType, content.Platform
	if content.PlatformUnknown {
		image.Platform = nil
	}
	image.Subject = content.Subject
	image.Kind = "manifest"
	if content.ArtifactType != "" {
		image.Kind = "artifact"
	}
	if content.MediaType == "application/vnd.oci.image.index.v1+json" || content.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		image.Kind = "index"
	}
	closure, err := s.inventoryClosure(ctx, image.Identity, image.ResolvedDigest)
	if err != nil {
		return err
	}
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	epoch := gate.Epoch
	image.Evidence = inventory.Evidence{ClosureComplete: closure.complete, CheckedAt: time.Now().UTC(), Freshness: "fresh", Reasons: closure.reasons, GCEpoch: epoch}
	if image.ResolvedDigest == "" {
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "binding_unresolved")
	}
	objects := make([]inventory.Object, 0, len(closure.objects))
	for _, ref := range closure.objects {
		object, _, err := s.inspectInventoryObject(ctx, image.Identity, ref, epoch)
		if err != nil {
			return err
		}
		object.Associated = true
		objects = append(objects, object)
		for _, reason := range object.Reasons {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, reason)
		}
	}
	var anchored bool
	image.Cache, image.Availability, anchored = summarizeInventoryObjects(objects, closure.complete)
	if !anchored {
		anchored, err = s.anchorInventoryPublication(ctx, image, objects)
		if err != nil {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "local_read_error")
		}
	}
	version := inventory.LifecycleVersion(image.ID, image.BindingRevision, image.ContentGeneration)
	image.Solidification.Lifecycle, err = s.inventory.Lifecycle(ctx, version)
	if err != nil {
		return err
	}
	if image.Solidification.Started == nil {
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "legacy_start_unknown")
	}
	if err := s.inventoryImageWork(ctx, &image, objects); err != nil {
		return err
	}
	if image.Scope == "reference" && image.Reference != image.ResolvedDigest && anchored {
		image.TagPublished, err = s.inventoryTagPublished(ctx, image)
		if err != nil {
			image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "local_read_error")
		}
	}
	if err := s.recheckInventoryImage(ctx, &image, content, objects, epoch); err != nil {
		return err
	}
	image.Category = inventory.Classify(image, anchored)
	if image.Scope == "reference" && image.ResolvedDigest != "" {
		if err := writer.RequirePlatform(ctx, image.Identity, content.ObjectRef); err != nil {
			return err
		}
	}
	for _, ref := range closure.required {
		if err := writer.RequirePlatform(ctx, image.Identity, ref); err != nil {
			return err
		}
	}
	return writer.AddImage(ctx, image, objects, closure.platforms)
}

func (s *Service) recheckInventoryImage(ctx context.Context, image *inventory.Image, content inventory.Content, objects []inventory.Object, epoch uint64) error {
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	invalid := content.Generation != image.ContentGeneration || gate.Epoch != epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier
	for _, object := range objects {
		generation, err := s.generation(ctx, image.Authority, image.Repository, object.Digest)
		if err != nil {
			return err
		}
		invalid = invalid || generation.Tombstone || generation.Generation != object.Generation
		if object.Digest == image.ResolvedDigest && object.Generation != image.ContentGeneration {
			invalid = true
		}
	}
	if image.Scope == "reference" {
		generation, err := s.generation(ctx, image.Authority, image.Repository, image.Reference)
		if err != nil {
			return err
		}
		invalid = invalid || generation.Tombstone || generation.Generation != image.Generation
	}
	if invalid {
		image.Evidence.Freshness = "unknown"
		image.Evidence.Reasons = appendReason(image.Evidence.Reasons, "generation_or_epoch_changed")
		image.Availability.State = "unknown"
	}
	return nil
}
