package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/workqueue"
	"time"
)

func (s *Service) buildInventoryPage(ctx context.Context, state *inventoryCheckpoint) error {
	if state.BuildPhase < 0 || state.BuildPhase > 5 {
		return storage.ErrCorruptObject
	}
	writer := s.inventory.ResumeWriter(*state.Build)
	if state.BuildPhase == 5 {
		return s.publishInventory(ctx, state, writer)
	}
	if state.BuildPhase == 4 {
		done, err := writer.AssembleStep(ctx, s.inventoryConfig().BatchSize)
		if err == nil && done {
			state.BuildPhase++
		}
		state.Build = &writer.Snapshot
		return err
	}
	if state.ImageBuild != nil {
		done, err := s.advanceInventoryImageUnit(ctx, writer, state.ImageBuild)
		if err == nil && done {
			state.ImageBuild = nil
			if state.BuildPhase != 3 {
				state.BuildPage.Advance()
			}
		}
		return err
	}
	prefixes := []string{"contents/", "bindings/", "cache/", "snapshots/" + state.Build.SnapshotID + "/pending/"}
	if state.BuildPage == nil {
		state.BuildPage = &inventory.Cursor{Token: state.BuildToken}
	}
	page := state.BuildPage
	entry, ok, err := page.Peek(ctx, s.store, inventory.Prefix+prefixes[state.BuildPhase], s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	if ok {
		if state.BuildPhase == 2 {
			err = s.buildInventoryCacheObject(ctx, writer, entry.Key)
			if err == nil || errors.Is(err, storage.ErrNotFound) {
				page.Advance()
			}
		} else {
			err = s.startInventoryImage(ctx, state, writer, entry.Key)
		}
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
	}
	state.BuildToken = page.Token
	if page.Done {
		if state.BuildPhase == 3 {
			entries, next, err := storage.Scan(ctx, s.store, inventory.Prefix+prefixes[3], "", 1)
			if err != nil {
				return err
			}
			state.BuildPage, state.BuildToken = nil, ""
			if len(entries) > 0 || next != "" {
				return nil
			}
		}
		state.BuildPhase++
		state.BuildPage, state.BuildToken = nil, ""
	}
	return nil
}

func (s *Service) startInventoryImage(ctx context.Context, state *inventoryCheckpoint, writer *inventory.Writer, key string) error {
	var image inventory.Image
	var content inventory.Content
	var err error
	switch state.BuildPhase {
	case 0:
		content, _, err = inventory.Read[inventory.Content](ctx, s.store, key)
		identity := content.Identity
		identity.Reference = content.Digest
		image = inventory.Image{ID: inventory.ID("platform", identity.Authority, identity.Repository, content.Digest), ContentID: identity.ContentID(content.Digest), Scope: "platform", Identity: identity, ResolvedDigest: content.Digest, ContentGeneration: content.Generation}
	case 1:
		var binding inventory.Binding
		binding, _, err = inventory.Read[inventory.Binding](ctx, s.store, key)
		if err == nil {
			content, err = s.inventory.Content(ctx, binding.Identity, binding.Digest)
			if errors.Is(err, storage.ErrNotFound) {
				content, err = inventory.Content{Identity: binding.Identity, ObjectRef: inventory.ObjectRef{Role: "manifest", Digest: binding.Digest}, Generation: binding.ContentGeneration}, nil
			}
			image = inventory.Image{ID: binding.Identity.ID(), ContentID: binding.ContentID(binding.Digest), Scope: "reference", Identity: binding.Identity, ResolvedDigest: binding.Digest, BindingRevision: binding.Revision, BindingSource: binding.Source, BindingConfirmedAt: binding.ConfirmedAt, Generation: binding.Generation, ContentGeneration: binding.ContentGeneration}
		}
	case 3:
		var request inventory.PlatformRequest
		request, _, err = inventory.Read[inventory.PlatformRequest](ctx, s.store, key)
		if err == nil {
			content, err = s.inventory.Content(ctx, request.Identity, request.Digest)
			if errors.Is(err, storage.ErrNotFound) {
				generation, getErr := s.generation(ctx, request.Authority, request.Repository, request.Digest)
				content, err = inventory.Content{Identity: request.Identity, ObjectRef: request.ObjectRef, Generation: generation.Generation}, getErr
			}
			image = inventory.Image{ID: inventory.ID("platform", request.Authority, request.Repository, request.Digest), ContentID: request.ContentID(request.Digest), Scope: "platform", Identity: request.Identity, ResolvedDigest: request.Digest, ContentGeneration: content.Generation}
		}
	}
	if errors.Is(err, storage.ErrNotFound) {
		state.BuildPage.Advance()
		return nil
	}
	if err != nil {
		return err
	}
	ready, err := writer.HasImage(ctx, image.ID)
	if err != nil {
		return err
	}
	if ready {
		if state.BuildPhase == 3 {
			if err := writer.CompletePlatform(ctx, key); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
		}
		state.BuildPage.Advance()
		return nil
	}
	state.ImageBuild, err = s.newInventoryImageProgress(ctx, image, content)
	return err
}

func (s *Service) publishInventory(ctx context.Context, state *inventoryCheckpoint, writer *inventory.Writer) error {
	if state.Build.ProjectionObservedAt.IsZero() {
		for _, kind := range []string{"inventory", "execution"} {
			entries, _, err := storage.Scan(ctx, s.store, workqueue.Prefix+"jobs/"+kind+"/", "", 1)
			if err != nil {
				return err
			}
			state.Build.ProjectionPending = state.Build.ProjectionPending || len(entries) > 0
		}
		state.Build.ProjectionObservedAt = time.Now().UTC()
		return nil
	}
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return err
	}
	if writer.Snapshot.GCEpoch != gate.Epoch {
		return storage.ErrConditionFailed
	}
	reasons := state.Scan.Reasons
	if writer.Snapshot.ProjectionPending {
		reasons = appendReason(reasons, "background_projection_pending")
	}
	if err := writer.Publish(ctx, state.Scan.Complete && !writer.Snapshot.ProjectionPending, reasons); err != nil {
		return err
	}
	state.LastPublishedAt = time.Now().UTC()
	state.LastBuildSeconds = state.LastPublishedAt.Sub(writer.Snapshot.BuildStartedAt).Seconds()
	if !state.ScanStartedAt.IsZero() {
		state.LastRefreshSeconds = state.LastPublishedAt.Sub(state.ScanStartedAt).Seconds()
	}
	if writer.Snapshot.Coverage == "complete" {
		state.LastCompleteAt = state.LastPublishedAt
	}
	state.ScanStartedAt = time.Time{}
	state.Build = nil
	state.Scan.Reasons = nil
	if s.metrics != nil {
		s.metrics.recordEvent("inventory_snapshot_published")
		s.metrics.inventoryPublished(writer.Snapshot)
	}
	return nil
}
