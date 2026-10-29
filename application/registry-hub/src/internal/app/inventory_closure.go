package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type inventoryClosure struct {
	objects   []inventory.ObjectRef
	platforms []string
	required  map[string]inventory.ObjectRef
	seen      map[string]inventory.ObjectRef
	active    map[string]bool
	complete  bool
	bytes     int64
	reasons   []string
}

func (s *Service) inventoryClosure(ctx context.Context, identity inventory.Identity, digest string) (inventoryClosure, error) {
	closure := inventoryClosure{complete: true, seen: map[string]inventory.ObjectRef{}, required: map[string]inventory.ObjectRef{}, active: map[string]bool{}, reasons: []string{}, platforms: []string{}}
	err := s.visitInventoryManifest(ctx, identity, digest, 0, &closure)
	return closure, err
}

func (s *Service) visitInventoryManifest(ctx context.Context, identity inventory.Identity, digest string, depth int, closure *inventoryClosure) error {
	if closure.active[digest] || depth > s.cfg.Limits.ClosureDepth {
		closure.complete = false
		closure.reasons = appendReason(closure.reasons, "closure_limit_or_cycle")
		return nil
	}
	if _, seen := closure.seen["manifest:"+digest]; seen {
		return nil
	}
	content, err := s.inventory.Content(ctx, identity, digest)
	if errors.Is(err, storage.ErrNotFound) {
		closure.complete = false
		closure.reasons = appendReason(closure.reasons, "closure_incomplete")
		return nil
	}
	if err != nil {
		return err
	}
	if len(closure.objects) >= s.cfg.Limits.ClosureObjects || content.Size < 0 || content.Size > s.cfg.Limits.ClosureBytes-closure.bytes {
		closure.complete = false
		closure.reasons = appendReason(closure.reasons, "closure_limit")
		return nil
	}
	closure.active[digest] = true
	defer delete(closure.active, digest)
	closure.objects = append(closure.objects, content.ObjectRef)
	closure.seen["manifest:"+digest] = content.ObjectRef
	closure.bytes += content.Size
	if !content.ClosureKnown {
		closure.complete = false
		closure.reasons = appendReason(closure.reasons, "closure_incomplete")
	}
	for _, child := range content.Children {
		if child.Role == "manifest" {
			known, err := s.inventory.Content(ctx, identity, child.Digest)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			if err == nil && known.ClosureKnown && (known.Size != child.Size || known.MediaType != child.MediaType) {
				closure.complete = false
				closure.reasons = appendReason(closure.reasons, "descriptor_conflict")
			}
			if _, seen := closure.seen["manifest:"+child.Digest]; seen {
				continue
			}
			if _, exists := closure.required[child.Digest]; !exists {
				closure.required[child.Digest] = child
				closure.platforms = append(closure.platforms, inventory.ID("platform", identity.Authority, identity.Repository, child.Digest))
			}
			if err := s.visitInventoryManifest(ctx, identity, child.Digest, depth+1, closure); err != nil {
				return err
			}
			continue
		}
		if previous, exists := closure.seen["blob:"+child.Digest]; exists {
			if previous.Size != child.Size {
				closure.complete = false
				closure.reasons = appendReason(closure.reasons, "descriptor_conflict")
			}
			continue
		}
		if len(closure.objects) >= s.cfg.Limits.ClosureObjects || child.Size < 0 || child.Size > s.cfg.Limits.ClosureBytes-closure.bytes {
			closure.complete = false
			closure.reasons = appendReason(closure.reasons, "closure_limit")
			continue
		}
		closure.objects = append(closure.objects, child)
		closure.seen["blob:"+child.Digest] = child
		closure.bytes += child.Size
	}
	return nil
}

func summarizeInventoryObjects(objects []inventory.Object, complete bool) (inventory.CacheState, inventory.Availability, bool) {
	cache := inventory.CacheState{State: "none"}
	availability := inventory.Availability{State: "partial"}
	expected, expectedBytes, missing, missingBytes := int64(len(objects)), int64(0), int64(0), int64(0)
	cacheUnknown, localUnknown, anchored := !complete, !complete, complete
	for _, object := range objects {
		expectedBytes += object.Size
		if object.CacheState == "complete" {
			cache.Objects.Cached++
			cache.LogicalBytes.Cached += object.Size
		}
		if object.CacheState == "unknown" {
			cacheUnknown = true
		}
		if object.DistributionVerified {
			cache.Objects.DistributionVerified++
			cache.LogicalBytes.DistributionVerified += object.Size
		}
		if object.CleanupPending && object.CacheState == "complete" {
			cache.Objects.CleanupPending++
			cache.LogicalBytes.CleanupPending += object.Size
		}
		if object.CacheState != "complete" && !object.DistributionVerified {
			missing++
			missingBytes += object.Size
		}
		if !object.Anchored {
			anchored = false
		}
		for _, reason := range object.Reasons {
			if reason == "local_evidence_stale" || reason == "local_evidence_invalid" || reason == "local_evidence_unavailable" || reason == "local_read_error" || reason == "storage_read_error" {
				localUnknown = true
			}
		}
	}
	if complete {
		cache.Objects.Expected = &expected
		cache.LogicalBytes.Expected = &expectedBytes
		if !localUnknown && !cacheUnknown {
			cache.Objects.Missing = &missing
			cache.LogicalBytes.Missing = &missingBytes
		}
	}
	if cacheUnknown {
		cache.State = "unknown"
	} else if cache.Objects.Cached == expected {
		cache.State = "complete"
	} else if cache.Objects.Cached > 0 {
		cache.State = "partial"
	}
	if complete && missing == 0 {
		availability.State = "complete"
	} else if localUnknown || cacheUnknown {
		availability.State = "unknown"
	}
	return cache, availability, anchored
}
