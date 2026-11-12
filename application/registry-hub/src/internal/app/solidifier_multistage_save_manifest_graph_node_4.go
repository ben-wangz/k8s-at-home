package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) saveManifestGraphNode(ctx context.Context, task *tasks.Task, reference contentRef, complete bool, state string) error {
	return s.saveGraphNodeState(ctx, task, reference, tasks.GraphManifest, complete, state)
}

func (s *solidifier) saveBlobGraphNode(ctx context.Context, task *tasks.Task, reference contentRef) error {
	return s.saveBlobGraphNodeState(ctx, task, reference, "discovered")
}

func (s *solidifier) saveStagedEdge(ctx context.Context, rootTaskID string, parentRole tasks.GraphRole, parentDigest string, order int, childRole tasks.GraphRole, descriptor artifact.Descriptor) error {
	entry := graphEntryDescriptor(descriptor)
	edge := tasks.DependencyEdge{RootTaskID: rootTaskID, ParentRole: parentRole, ParentDigest: parentDigest, Order: order, ChildRole: childRole, ChildDigest: descriptor.Digest, Descriptor: entry}
	if err := s.service.records.SaveDependencyEdge(ctx, edge); err != nil {
		return withSolidificationFailureStage(temporary("task_state", err), "task_progress_commit")
	}
	return nil
}

func graphEntry(reference contentRef) tasks.GraphEntry {
	return tasks.GraphEntry{Digest: reference.digest, MediaType: reference.mediaType, Size: reference.size, ArtifactType: reference.artifactType, Annotations: cloneAnnotations(reference.annotations), Platform: append([]byte(nil), reference.platform...), URLs: append([]string(nil), reference.urls...)}
}

func graphEntryDescriptor(descriptor artifact.Descriptor) tasks.GraphEntry {
	entry := graphEntry(descriptorRef(descriptor))
	entry.Data = append([]byte(nil), descriptor.Data...)
	return entry
}

func (s *solidifier) assignStagedBlobScopes(ctx context.Context, task *tasks.Task, graph *stagedGraph) error {
	for _, manifest := range graph.manifests {
		scopeID, err := protectionScopeID(task.Authority, task.Repository, manifest.ref.digest, task.Generation)
		if err != nil {
			return permanent("task_state", err)
		}
		manifest.scopeID = scopeID
		manifest.sessionID = stagedSessionID(scopeID)
		manifest.scopeBytes = manifest.ref.size
		unique := make(map[string]bool, len(manifest.blobs))
		for _, blob := range manifest.blobs {
			if !unique[blob.digest] {
				manifest.scopeBytes += blob.size
				unique[blob.digest] = true
			}
		}
		for _, blob := range manifest.blobs {
			intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return temporary("task_state", err)
			}
			generation, err := s.contentGeneration(ctx, task.Authority, task.Repository, blob.digest)
			if err != nil {
				return temporary("generation", err)
			}
			if intent.Generation != generation {
				continue
			}
			demandID := stagedDemandID(task.ID, manifest.scopeID)
			if err := s.service.records.LinkBlobDemandScope(ctx, intent.ID, demandID, task.ID, manifest.scopeID, manifest.sessionID, manifest.scopeBytes); err != nil {
				return temporary("task_state", err)
			}
		}
	}
	return nil
}

func (s *solidifier) distributionBlobPresent(ctx context.Context, task *tasks.Task, blob contentRef, sessionID string) (bool, error) {
	leaseCtx, release, err := acquireContentLease(ctx, s.tasks.Leases(), task.Authority, task.Repository, blob.digest, task.ID)
	if err != nil {
		if errors.Is(err, lease.ErrHeld) {
			return false, temporary("object_busy", err)
		}
		return false, temporary("object_lease", err)
	}
	defer release()
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/blobs/" + blob.digest
	response, err := s.registry.Do(leaseCtx, http.MethodHead, path, nil, make(http.Header), nil, 0)
	if err != nil {
		return false, temporary("distribution", err)
	}
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodHead, route.EndpointBlob)
		_ = response.Body.Close()
		if miss {
			return false, nil
		}
		return false, temporary("distribution", errors.New("Distribution returned an untrusted blob miss"))
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, temporary("distribution", fmt.Errorf("Distribution blob HEAD returned status %d", response.StatusCode))
	}
	if response.ContentLength != blob.size || response.Header.Get("Docker-Content-Digest") != blob.digest {
		return false, permanent("distribution_invalid", errors.New("existing Distribution blob does not match its descriptor"))
	}
	if sessionID != "" {
		if err := s.gate.MarkProtectionContentObserved(leaseCtx, sessionID); err != nil {
			return false, temporary("protection_state", err)
		}
	}
	if err := s.observeSolidificationBlob(leaseCtx, task, blob, sessionID, "distribution_head_digest"); err != nil {
		return false, err
	}
	if err := s.saveBlobGraphNodeState(ctx, task, blob, "verified_present"); err != nil {
		return false, err
	}
	return true, nil
}

func (s *solidifier) verifyOrReuseLocalBlob(ctx context.Context, task *tasks.Task, blob contentRef, sessionID, path string) error {
	generation, err := s.blobContentGeneration(ctx, task, blob.digest)
	if err != nil {
		return temporary("generation", err)
	}
	epoch := s.gate.State().Epoch
	return s.verifyOrReuseLocalBlobAtGenerationEpoch(ctx, task, blob, sessionID, path, generation, epoch)
}

func (s *solidifier) verifyOrReuseLocalBlobAtGenerationEpoch(ctx context.Context, task *tasks.Task, blob contentRef, sessionID, path string, generation, epoch uint64) error {
	return s.coordinateBlobVerification(ctx, task, blob, sessionID, path, generation, epoch)
}

func (s *solidifier) recordVerifiedBlobObservation(ctx context.Context, task *tasks.Task, blob contentRef, sessionID string, generation, epoch uint64) error {
	return s.recordBlobObservation(ctx, task, blob, sessionID, generation, epoch, "distribution_readback")
}

func (s *solidifier) recordBlobObservation(ctx context.Context, task *tasks.Task, blob contentRef, sessionID string, generation, epoch uint64, basis string) error {
	if err := s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch); err != nil {
		return err
	}
	ownership, owned := ctx.Value(contentLeaseContextKey{}).(contentLeaseOwnership)
	if !owned || ownership.holder.Key != "leases/objects/"+objectLeaseKey(task.Authority, task.Repository, blob.digest) {
		return temporary("object_lease", errors.New("solidification observation requires the content lease"))
	}
	if err := ownership.manager.Validate(ctx, ownership.holder); err != nil {
		return temporary("object_lease", err)
	}
	owner := sessionID
	if owner == "" {
		owner = task.ID
	}
	operationID := fmt.Sprintf("%s-%s-%d-%d-%s", owner, objectLeaseKey(task.Authority, task.Repository, blob.digest), generation, epoch, basis)
	guarded := inventory.WithWriteGuard(ctx, func(check context.Context) error {
		if err := ownership.manager.Validate(check, ownership.holder); err != nil {
			return err
		}
		return s.requireStableObservationEpoch(check, task, blob.digest, generation, epoch)
	})
	if err := s.service.records.RecordLocalObservation(guarded, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphBlob,
		Digest: blob.digest, ActualSize: blob.size, Generation: generation,
		GCEpoch: epoch, OperationID: operationID, VerificationBasis: basis,
	}); err != nil {
		return temporary("task_state", err)
	}
	return s.requireStableObservationEpoch(ctx, task, blob.digest, generation, epoch)
}
