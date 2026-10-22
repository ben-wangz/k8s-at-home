package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

func (s *solidifier) saveManifestGraphNode(ctx context.Context, task *tasks.Task, reference contentRef, complete bool, state string) error {
	node := tasks.GraphNode{RootTaskID: task.ID, Role: tasks.GraphManifest, Digest: reference.digest, MediaType: reference.mediaType, DeclaredSize: reference.size, ActualSize: reference.size, DiscoveryComplete: complete, LocalState: state, Generation: task.Generation, GCEpoch: s.gate.State().Epoch}
	if existing, err := s.service.records.GetGraphNode(ctx, task.ID, tasks.GraphManifest, reference.digest); err == nil {
		node = existing
		node.MediaType = reference.mediaType
		node.DeclaredSize = reference.size
		node.ActualSize = reference.size
		node.DiscoveryComplete = complete
		node.LocalState = state
		node.Generation = task.Generation
		node.GCEpoch = s.gate.State().Epoch
	} else if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	if err := s.service.records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, reference.digest); err != nil {
		return temporary("task_state", err)
	}
	if err := s.service.records.SaveGraphNode(ctx, &node); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func (s *solidifier) saveBlobGraphNode(ctx context.Context, task *tasks.Task, reference contentRef) error {
	node := tasks.GraphNode{RootTaskID: task.ID, Role: tasks.GraphBlob, Digest: reference.digest, MediaType: reference.mediaType, DeclaredSize: reference.size, ActualSize: reference.size, DiscoveryComplete: true, LocalState: "discovered", Generation: task.Generation, GCEpoch: s.gate.State().Epoch}
	if existing, err := s.service.records.GetGraphNode(ctx, task.ID, tasks.GraphBlob, reference.digest); err == nil {
		node = existing
		node.MediaType = reference.mediaType
		node.DeclaredSize = reference.size
		node.ActualSize = reference.size
		node.DiscoveryComplete = true
		if node.LocalState == "" {
			node.LocalState = "discovered"
		}
		node.Generation = task.Generation
		node.GCEpoch = s.gate.State().Epoch
	} else if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	if err := s.service.records.EnsureCacheProtectionReference(ctx, task.ID, task.Authority, task.Repository, reference.digest); err != nil {
		return temporary("task_state", err)
	}
	if err := s.service.records.SaveGraphNode(ctx, &node); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func (s *solidifier) saveStagedEdge(ctx context.Context, rootTaskID string, parentRole tasks.GraphRole, parentDigest string, order int, childRole tasks.GraphRole, descriptor artifact.Descriptor) error {
	entry := graphEntryDescriptor(descriptor)
	edge := tasks.DependencyEdge{RootTaskID: rootTaskID, ParentRole: parentRole, ParentDigest: parentDigest, Order: order, ChildRole: childRole, ChildDigest: descriptor.Digest, Descriptor: entry}
	if err := s.service.records.SaveDependencyEdge(ctx, edge); err != nil {
		return temporary("task_state", err)
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
			generation, err := s.blobContentGeneration(ctx, task, blob.digest)
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
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/blobs/" + blob.digest
	response, err := s.registry.Do(ctx, http.MethodHead, path, nil, make(http.Header), nil, 0)
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
		if err := s.gate.MarkProtectionContentObserved(ctx, sessionID); err != nil {
			return false, temporary("protection_state", err)
		}
	}
	if err := s.verifyOrReuseLocalBlob(ctx, task, blob, sessionID, path); err != nil {
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
	_, err = s.service.records.FindLocalObservation(ctx, task.Authority, task.Repository, tasks.GraphBlob, blob.digest, blob.size, generation, epoch)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrNotFound) {
		return temporary("task_state", err)
	}
	if err := s.verifyLocalObject(ctx, path, blob.digest, blob.size); err != nil {
		return err
	}
	owner := sessionID
	if owner == "" {
		owner = task.ID
	}
	operationID := fmt.Sprintf("%s-%s-%d-%d", owner, objectLeaseKey(task.Authority, task.Repository, blob.digest), generation, epoch)
	if err := s.service.records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphBlob,
		Digest: blob.digest, ActualSize: blob.size, Generation: generation,
		GCEpoch: epoch, OperationID: operationID, VerificationBasis: "distribution_readback",
	}); err != nil {
		return temporary("task_state", err)
	}
	return nil
}
