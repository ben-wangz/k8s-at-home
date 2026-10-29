package app

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"registry-hub/internal/inventory"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"time"
)

func (s *Service) inspectInventoryObject(ctx context.Context, identity inventory.Identity, ref inventory.ObjectRef, epoch uint64) (inventory.Object, bool, error) {
	object := inventory.Object{ID: identity.ContentID(ref.Digest), Identity: identity, ObjectRef: ref, CacheState: "none", CheckedAt: time.Now().UTC(), Reasons: []string{}, TaskIDs: []string{}}
	generation, err := s.generation(ctx, identity.Authority, identity.Repository, ref.Digest)
	if err != nil {
		return object, false, err
	}
	object.Generation = generation.Generation
	if err := s.inventoryObjectWork(ctx, &object); err != nil {
		object.Reasons = appendReason(object.Reasons, "storage_read_error")
	}
	if generation.Tombstone {
		object.Reasons = append(object.Reasons, "content_deleted")
		return object, false, nil
	}
	inspection, err := s.cache.InspectAny(ctx, identity.Authority, identity.Repository, ref.Digest, ref.MediaType)
	if err != nil {
		object.CacheState = "unknown"
		object.Reasons = append(object.Reasons, "storage_read_error")
	} else {
		object.DataVersion = inspection.DataVersion
		object.CleanupPending = inspection.State == "cleanup_pending"
		if inspection.Present && inspection.Size == ref.Size {
			object.CacheState = "complete"
		} else if inspection.Unknown {
			object.CacheState = "unknown"
		}
		if inspection.Reason != "" {
			object.Reasons = append(object.Reasons, inspection.Reason)
		}
	}
	role := tasks.GraphBlob
	if ref.Role == "manifest" {
		role = tasks.GraphManifest
	}
	proof, _, proofErr := inventory.Read[inventoryProof](ctx, s.store, inventory.Prefix+"proofs/"+object.ID+".json")
	if proofErr != nil && !errors.Is(proofErr, storage.ErrNotFound) {
		object.Reasons = append(object.Reasons, "storage_read_error")
		return object, true, nil
	}
	_, observationErr := s.records.FindLocalObservation(ctx, identity.Authority, identity.Repository, role, ref.Digest, ref.Size, generation.Generation, epoch)
	if identity.Authority == "" {
		observationErr = storage.ErrNotFound
	}
	validObservation := observationErr == nil
	validProof := proofErr == nil && proof.SchemaVersion == 1 && !proof.ObservedAt.IsZero() && proof.Generation == generation.Generation && proof.Epoch == epoch
	if !validObservation && observationErr != nil && !errors.Is(observationErr, storage.ErrNotFound) {
		object.Reasons = append(object.Reasons, "storage_read_error")
		return object, true, nil
	}
	if s.registry == nil {
		object.Reasons = append(object.Reasons, "local_evidence_unavailable")
		return object, true, nil
	}
	internal, err := inventoryInternalName(identity)
	if err != nil {
		return object, true, err
	}
	path := "/v2/" + internal + "/blobs/" + ref.Digest
	endpoint := route.EndpointBlob
	if ref.Role == "manifest" {
		path = "/v2/" + internal + "/manifests/" + url.PathEscape(ref.Digest)
		endpoint = route.EndpointManifest
	}
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		object.Reasons = append(object.Reasons, "local_read_error")
		return object, true, nil
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && confirmedMiss(response, http.MethodHead, endpoint) {
		return object, false, nil
	}
	if response.StatusCode != http.StatusOK || response.ContentLength != ref.Size || response.Header.Get("Docker-Content-Digest") != ref.Digest {
		object.Reasons = append(object.Reasons, "local_evidence_invalid")
		return object, true, nil
	}
	if !validObservation && !validProof {
		if pending, ok := ctx.Value(inventoryAuditCapture{}).(**inventoryAuditProgress); ok {
			*pending = &inventoryAuditProgress{Object: object, Path: path, Epoch: epoch}
			return object, true, nil
		}
		if err := s.auditInventoryObject(ctx, object, path, epoch); err != nil {
			object.Reasons = append(object.Reasons, "local_evidence_stale")
			return object, true, nil
		}
	}
	object.DistributionVerified = true
	object.Anchored = validProof && proof.State == "anchored"
	return object, false, nil
}

func (s *Service) buildInventoryCacheObject(ctx context.Context, writer *inventory.Writer, key string) error {
	object, _, err := inventory.Read[inventory.Object](ctx, s.store, key)
	if err != nil {
		return err
	}
	inspection, err := s.cache.InspectAny(ctx, object.Authority, object.Repository, object.Digest, object.MediaType)
	if err != nil {
		object.CacheState = "unknown"
		object.Reasons = []string{"storage_read_error"}
	} else {
		object.CacheState = "none"
		if inspection.Reason == "cache_metadata_missing" {
			return nil
		}
		if inspection.Present {
			object.CacheState = "complete"
		}
		if inspection.Unknown {
			object.CacheState = "unknown"
		}
		object.CleanupPending = inspection.State == "cleanup_pending"
		object.DataVersion = inspection.DataVersion
		object.Reasons = []string{}
		if inspection.Reason != "" {
			object.Reasons = append(object.Reasons, inspection.Reason)
		}
	}
	links, _, err := storage.Scan(ctx, s.store, inventory.Prefix+"object-links/"+object.ID+"/", "", 1)
	if err != nil {
		return err
	}
	object.Associated = len(links) > 0
	if !object.Associated {
		object.Reasons = append(object.Reasons, "manifest_association_unknown")
	}
	object.CheckedAt = time.Now().UTC()
	generation, err := s.generation(ctx, object.Authority, object.Repository, object.Digest)
	if err != nil {
		return err
	}
	object.Generation = generation.Generation
	if err := s.inventoryObjectWork(ctx, &object); err != nil {
		object.Reasons = appendReason(object.Reasons, "storage_read_error")
	}
	return writer.AddCacheObject(ctx, object)
}
