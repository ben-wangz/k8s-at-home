package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"sort"
	"time"
)

type stagedManifest struct {
	ref        contentRef
	blobs      []contentRef
	children   []contentRef
	scopeID    string
	sessionID  string
	scopeBytes int64
}

type stagedGraph struct {
	root       contentRef
	manifests  []*stagedManifest
	byManifest map[string]*stagedManifest
	blobs      map[string]contentRef
	active     map[string]bool
	objects    int
	bytes      int64
}

func (s *solidifier) processRootMultiStage(ctx context.Context, task *tasks.Task, taskLease lease.Lease) error {
	if task.RootDigest == "" || task.MediaType == "" {
		return permanent("task_state", errors.New("root task is missing its discovered manifest identity"))
	}
	graph := &stagedGraph{byManifest: make(map[string]*stagedManifest), blobs: make(map[string]contentRef), active: make(map[string]bool)}
	root := contentRef{digest: task.RootDigest, mediaType: task.MediaType, size: -1}
	rootManifest, err := s.discoverStagedManifest(ctx, task, taskLease, graph, root, 0, true)
	if err != nil {
		return err
	}
	graph.root = rootManifest.ref
	scopeID, err := protectionScopeID(task.Authority, task.Repository, task.RootDigest, task.Generation)
	if err != nil {
		return permanent("task_state", err)
	}
	rootEntry := graphEntry(graph.root)
	previousRoot, hasPreviousRoot := task.Graph[task.RootDigest]
	progressed := task.ScopeID != scopeID || task.Discovery != "complete" || task.RequestedBytes != graph.bytes || !hasPreviousRoot || !reflect.DeepEqual(previousRoot, rootEntry)
	task.ScopeID = scopeID
	task.Discovery = "complete"
	task.RequestedBytes = graph.bytes
	if task.Graph == nil {
		task.Graph = make(map[string]tasks.GraphEntry)
	}
	task.Graph[task.RootDigest] = rootEntry
	if progressed {
		task.GraphVersion++
		if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
			return temporary("task_state", err)
		}
	}
	if err := s.assignStagedBlobScopes(ctx, task, graph); err != nil {
		return err
	}
	manifestReady := make(map[string]bool, len(graph.manifests))
	missing := false
	for _, manifest := range graph.manifests {
		childrenReady := true
		for _, child := range manifest.children {
			if !manifestReady[child.digest] {
				childrenReady = false
				break
			}
		}
		if !childrenReady {
			missing = true
			continue
		}
		cacheReadyByDigest := make(map[string]bool, len(manifest.blobs))
		intentByDigest := make(map[string]tasks.BlobIntent, len(manifest.blobs))
		for _, blob := range manifest.blobs {
			cacheReady, intent, err := s.prepareStagedBlob(ctx, task, manifest, blob)
			if err != nil {
				return err
			}
			if intent.CacheVersion != "" && intent.ActualSize != blob.size {
				return permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
			}
			cacheReadyByDigest[blob.digest] = cacheReady
			intentByDigest[blob.digest] = intent
		}
		maxSessions, maxBytes := protectionLimits(s.cfg)
		if manifest.scopeBytes > maxBytes {
			return permanent("capacity_limit", errors.New("manifest scope exceeds the unanchored Distribution byte limit"))
		}
		admitted, err := s.admitManifestScope(ctx, task, taskLease, manifest, graph.bytes, maxSessions, maxBytes, false)
		if err != nil {
			return withSolidificationFailureStage(err, "manifest_scope_admission")
		}
		if !admitted {
			if _, _, err := s.readDistributionManifest(ctx, task, manifest.ref); err == nil {
				if err := s.finalizeAnchoredManifestBlobs(ctx, task, manifest); err != nil {
					return err
				}
				manifestReady[manifest.ref.digest] = true
				continue
			} else if !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			admitted, err = s.admitManifestScope(ctx, task, taskLease, manifest, graph.bytes, maxSessions, maxBytes, true)
			if err != nil {
				return withSolidificationFailureStage(err, "manifest_scope_recovery")
			}
			if !admitted {
				if _, _, err := s.readDistributionManifest(ctx, task, manifest.ref); err != nil {
					if errors.Is(err, storage.ErrNotFound) {
						return temporary("protection_state", errors.New("anchored manifest remained missing after protection recovery"))
					}
					return err
				}
				if err := s.finalizeAnchoredManifestBlobs(ctx, task, manifest); err != nil {
					return err
				}
				manifestReady[manifest.ref.digest] = true
				continue
			}
		}
		ready := true
		for _, blob := range manifest.blobs {
			cacheReady := cacheReadyByDigest[blob.digest]
			intent := intentByDigest[blob.digest]
			present, err := s.distributionBlobPresent(ctx, task, blob, manifest.sessionID)
			if err != nil {
				return withSolidificationFailureStage(err, "blob_presence_check")
			}
			if present {
				if cacheReady {
					if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, blob.digest); err != nil {
						return temporary("storage", err)
					}
				}
				continue
			}
			if cacheReady {
				blobTask, _, enqueueErr := s.service.enqueueBlobIntent(ctx, intent)
				if enqueueErr != nil && !errors.Is(enqueueErr, storage.ErrConditionFailed) {
					if s.service.logger != nil {
						s.service.logger.Warn("cached blob task admission deferred", "task_id", task.ID, "error_class", "queue")
					}
				} else if blobTask.Status == tasks.WaitingDeps && blobTask.WaitReason == "manifest_scope_admission" || blobTask.Status == tasks.VerifiedLocal {
					_, _ = s.tasks.Wake(ctx, intent.ID)
				}
			}
			ready = false
		}
		if !ready {
			missing = true
			if err := s.releaseEmptyManifestScope(ctx, task, taskLease, manifest.sessionID); err != nil {
				return err
			}
			continue
		}
		previousSession := task.ProtectionSessionID
		task.ProtectionSessionID = manifest.sessionID
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
		err = s.ensureManifest(requestCtx, task, taskLease, manifest.ref, manifest.ref.digest)
		cancel()
		task.ProtectionSessionID = previousSession
		if err != nil {
			return withSolidificationFailureStage(err, "manifest_publication")
		}
		if err := s.recordStagedManifestObservation(ctx, task, manifest.ref); err != nil {
			return err
		}
		if err := s.markStagedManifestPublished(ctx, task, manifest); err != nil {
			return err
		}
		if manifest.ref.digest == task.RootDigest && !isDigestReference(task.Reference) {
			manifestReady[manifest.ref.digest] = true
			continue
		}
		if err := s.anchorManifestScope(ctx, task, taskLease, manifest); err != nil {
			return withSolidificationFailureStage(err, "manifest_anchor")
		}
		if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, manifest.ref.digest); err != nil {
			return temporary("storage", err)
		}
		manifestReady[manifest.ref.digest] = true
	}
	if missing || !manifestReady[task.RootDigest] {
		return tasks.WaitFor(tasks.WaitingDeps, "dependencies_unavailable")
	}
	if !isDigestReference(task.Reference) {
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.Limits.RequestTimeout)
		previousSession := task.ProtectionSessionID
		if rootTaskHasProtection(task, rootManifest.sessionID) {
			task.ProtectionSessionID = rootManifest.sessionID
		} else {
			task.ProtectionSessionID = ""
		}
		err := s.publishRootTag(requestCtx, task, taskLease)
		task.ProtectionSessionID = previousSession
		cancel()
		if err != nil {
			return withSolidificationFailureStage(err, "root_tag_publication")
		}
		if err := s.cache.MarkBlobCleanupPending(ctx, task.Authority, task.Repository, task.RootDigest); err != nil {
			return temporary("storage", err)
		}
	}
	if rootTaskHasProtection(task, rootManifest.sessionID) {
		if err := s.anchorManifestScope(ctx, task, taskLease, rootManifest); err != nil {
			return withSolidificationFailureStage(err, "root_manifest_anchor")
		}
	}
	s.wakeOldestQuotaTask(ctx)
	return nil
}

func (s *solidifier) discoverStagedManifest(ctx context.Context, task *tasks.Task, taskLease lease.Lease, graph *stagedGraph, reference contentRef, depth int, root bool) (*stagedManifest, error) {
	if depth > s.cfg.Limits.ClosureDepth {
		return nil, permanent("closure_limit", errors.New("manifest recursion depth exceeds limit"))
	}
	if !root && reference.size <= 0 {
		return nil, permanent("manifest_invalid", errors.New("manifest descriptor size must be positive"))
	}
	if graph.active[reference.digest] {
		return nil, permanent("manifest_cycle", errors.New("manifest graph contains a cycle"))
	}
	if existing, ok := graph.byManifest[reference.digest]; ok {
		if reference.size > 0 && existing.ref.size != reference.size || reference.mediaType != "" && !sameMediaType(reference.mediaType, existing.ref.mediaType) {
			return nil, permanent("manifest_invalid", errors.New("manifest descriptors disagree for the same digest"))
		}
		return existing, nil
	}
	if err := artifactDigest(reference.digest); err != nil {
		return nil, permanent("manifest_invalid", err)
	}
	data, actual, err := s.readManifest(ctx, task.Authority, task.Repository, reference)
	if errors.Is(err, storage.ErrNotFound) {
		data, actual, err = s.readDistributionManifest(ctx, task, reference)
		if errors.Is(err, storage.ErrNotFound) {
			if fetchErr := s.fetchObject(ctx, task, taskLease, reference, true); fetchErr != nil {
				return nil, fetchErr
			}
			data, actual, err = s.readManifest(ctx, task.Authority, task.Repository, reference)
		}
	}
	if err != nil {
		return nil, err
	}
	if !root && int64(len(data)) != reference.size {
		return nil, permanent("manifest_invalid", errors.New("manifest descriptor size mismatch"))
	}
	if reference.mediaType != "" && !sameMediaType(reference.mediaType, actual.mediaType) {
		return nil, permanent("manifest_invalid", errors.New("manifest descriptor media type mismatch"))
	}
	actual.artifactType = reference.artifactType
	actual.annotations = cloneAnnotations(reference.annotations)
	actual.platform = append([]byte(nil), reference.platform...)
	actual.urls = append([]string(nil), reference.urls...)
	if err := graph.add(actual, s.cfg.Limits); err != nil {
		return nil, err
	}
	parsed, err := artifact.ParseManifest(data, actual.mediaType, s.cfg.Limits.MaxDescriptors, s.cfg.Limits.DescriptorDataBytes)
	if err != nil {
		return nil, permanent("manifest_invalid", err)
	}
	node := &stagedManifest{ref: actual}
	graph.active[reference.digest] = true
	graph.byManifest[reference.digest] = node
	if err := s.saveManifestGraphNode(ctx, task, actual, false, "discovering"); err != nil {
		return nil, err
	}
	order := 0
	descriptors := append(copyDescriptors(parsed.Config), parsed.Layers...)
	for _, descriptor := range descriptors {
		blob := descriptorRef(descriptor)
		if err := s.validateStagedBlob(graph, blob); err != nil {
			return nil, err
		}
		if hasEmbeddedData(descriptor.Data) {
			if err := s.cachePut(ctx, task, taskLease, blob.digest, blob.mediaType, bytes.NewReader(descriptor.Embedded), blob.size); err != nil {
				return nil, temporary("storage", err)
			}
		}
		node.blobs = append(node.blobs, blob)
		if err := s.saveBlobGraphNode(ctx, task, blob); err != nil {
			return nil, err
		}
		if err := s.saveStagedEdge(ctx, task.ID, tasks.GraphManifest, actual.digest, order, tasks.GraphBlob, descriptor); err != nil {
			return nil, err
		}
		order++
	}
	if parsed.Manifests != nil {
		for _, descriptor := range *parsed.Manifests {
			childRef := descriptorRef(descriptor)
			if hasEmbeddedData(descriptor.Data) {
				if err := s.cachePut(ctx, task, taskLease, childRef.digest, childRef.mediaType, bytes.NewReader(descriptor.Embedded), childRef.size); err != nil {
					return nil, temporary("storage", err)
				}
			}
			if err := s.saveStagedEdge(ctx, task.ID, tasks.GraphManifest, actual.digest, order, tasks.GraphManifest, descriptor); err != nil {
				return nil, err
			}
			order++
			child, err := s.discoverStagedManifest(ctx, task, taskLease, graph, childRef, depth+1, false)
			if err != nil {
				return nil, err
			}
			node.children = append(node.children, child.ref)
		}
	}
	if parsed.Subject != nil && hasEmbeddedData(parsed.Subject.Data) {
		subject := descriptorRef(*parsed.Subject)
		if err := s.cachePut(ctx, task, taskLease, subject.digest, subject.mediaType, bytes.NewReader(parsed.Subject.Embedded), subject.size); err != nil {
			return nil, temporary("storage", err)
		}
	}
	graph.active[reference.digest] = false
	if err := s.saveManifestGraphNode(ctx, task, actual, true, "discovered"); err != nil {
		return nil, err
	}
	graph.manifests = append(graph.manifests, node)
	return node, nil
}

func (g *stagedGraph) add(reference contentRef, limits config.Limits) error {
	if reference.size < 0 || reference.size > math.MaxInt64-g.bytes {
		return permanent("manifest_invalid", errors.New("artifact size is invalid"))
	}
	g.objects++
	g.bytes += reference.size
	if g.objects > limits.ClosureObjects || g.bytes > limits.ClosureBytes {
		return permanent("closure_limit", errors.New("artifact closure exceeds configured limits"))
	}
	return nil
}

func (g *stagedGraph) addBlob(reference contentRef, limits config.Limits) error {
	if existing, ok := g.blobs[reference.digest]; ok {
		if existing.size != reference.size || !sameMediaType(existing.mediaType, reference.mediaType) {
			return permanent("manifest_invalid", errors.New("blob descriptors disagree for the same digest"))
		}
		return nil
	}
	if reference.size > limits.ObjectBytes {
		return permanent("object_limit", errors.New("descriptor size exceeds object limit"))
	}
	if err := g.add(reference, limits); err != nil {
		return err
	}
	g.blobs[reference.digest] = reference
	return nil
}

func (g *stagedGraph) addManifest(reference contentRef, limits config.Limits) error {
	if existing, ok := g.byManifest[reference.digest]; ok {
		if existing.ref.size != reference.size || !sameMediaType(existing.ref.mediaType, reference.mediaType) {
			return permanent("manifest_invalid", errors.New("manifest descriptors disagree for the same digest"))
		}
		return nil
	}
	return g.add(reference, limits)
}

func (s *solidifier) validateStagedBlob(graph *stagedGraph, reference contentRef) error {
	if reference.size < 0 || reference.size > s.cfg.Limits.ObjectBytes {
		return permanent("object_limit", errors.New("blob descriptor size is invalid or exceeds the object limit"))
	}
	if existing, ok := graph.blobs[reference.digest]; ok {
		if existing.size != reference.size || !sameMediaType(existing.mediaType, reference.mediaType) {
			return permanent("manifest_invalid", errors.New("blob descriptors disagree for the same digest"))
		}
		return nil
	}
	if err := graph.addBlob(reference, s.cfg.Limits); err != nil {
		return err
	}
	return nil
}

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

func (s *solidifier) readDistributionManifest(ctx context.Context, task *tasks.Task, reference contentRef) ([]byte, contentRef, error) {
	path := "/v2/" + s.internalName(task.Authority, task.Repository) + "/manifests/" + url.PathEscape(reference.digest)
	response, err := s.registry.Do(ctx, http.MethodGet, path, nil, manifestReadHeaders(), nil, 0)
	if err != nil {
		return nil, contentRef{}, temporary("distribution", err)
	}
	if response.StatusCode == http.StatusNotFound {
		miss := confirmedMiss(response, http.MethodGet, route.EndpointManifest)
		_ = response.Body.Close()
		if miss {
			return nil, contentRef{}, storage.ErrNotFound
		}
		return nil, contentRef{}, temporary("distribution", errors.New("Distribution returned an untrusted manifest miss"))
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, contentRef{}, temporary("distribution", fmt.Errorf("Distribution manifest GET returned status %d", response.StatusCode))
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, s.cfg.Limits.ManifestBytes+1))
	mediaType := response.Header.Get("Content-Type")
	headerDigest := response.Header.Get("Docker-Content-Digest")
	_ = response.Body.Close()
	if readErr != nil || int64(len(data)) > s.cfg.Limits.ManifestBytes || response.ContentLength >= 0 && int64(len(data)) != response.ContentLength || artifact.VerifyDigest(reference.digest, data) != nil || headerDigest != "" && headerDigest != reference.digest {
		return nil, contentRef{}, permanent("distribution_invalid", errors.New("existing Distribution manifest failed digest or size verification"))
	}
	if reference.size > 0 && reference.size != int64(len(data)) || reference.mediaType != "" && !sameMediaType(reference.mediaType, mediaType) {
		return nil, contentRef{}, permanent("distribution_invalid", errors.New("existing Distribution manifest differs from its descriptor"))
	}
	return data, contentRef{digest: reference.digest, mediaType: mediaType, size: int64(len(data))}, nil
}

func (s *solidifier) admitManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest, requestedBytes int64, maxSessions int, maxBytes int64, reopenAnchored bool) (bool, error) {
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
	if manifest.scopeBytes > maxBytes {
		return false, permanent("capacity_limit", errors.New("manifest scope exceeds the unanchored Distribution byte limit"))
	}
	_, alreadyAdmitted, err := s.gate.protectionSession(ctx, manifest.sessionID)
	if err != nil {
		return false, temporary("protection_state", err)
	}
	if !alreadyAdmitted {
		existing, readErr := s.gate.readProtectionSession(ctx, manifest.sessionID)
		if readErr == nil && (existing.State == "anchored" || existing.State == "released" && existing.TerminalReason == "anchored") {
			if !reopenAnchored {
				clearQuotaWaitHints(task)
				return false, nil
			}
		}
		if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
			return false, temporary("protection_state", readErr)
		}
	}
	oldSessions := make(map[string]struct{})
	var replaceableBytes int64
	replaceableSlots := 0
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return false, temporary("task_state", err)
		}
		if intent.ProtectionSessionID == "" || intent.ProtectionSessionID == manifest.sessionID {
			continue
		}
		session, active, err := s.gate.protectionSession(ctx, intent.ProtectionSessionID)
		if err != nil {
			return false, temporary("protection_state", err)
		}
		directScope, scopeErr := protectionScopeID(task.Authority, task.Repository, blob.digest, task.Generation)
		if scopeErr != nil {
			return false, permanent("task_state", scopeErr)
		}
		if active && session.ScopeID == directScope {
			_, seen := oldSessions[session.ID]
			oldSessions[session.ID] = struct{}{}
			if !seen && session.ReservationClass == "new" && (session.State == "protected" || session.State == "anchoring") && session.PendingOperations == 0 && !session.UncertainRemoteResult {
				if session.ReservedBytes > math.MaxInt64-replaceableBytes {
					return false, temporary("protection_state", errors.New("replaceable protection reservation overflows"))
				}
				replaceableBytes += session.ReservedBytes
				replaceableSlots++
			}
		}
	}
	oldIDs := make([]string, 0, len(oldSessions))
	for id := range oldSessions {
		oldIDs = append(oldIDs, id)
	}
	sort.Strings(oldIDs)
	if !alreadyAdmitted {
		task.QuotaWaitBytes = manifest.scopeBytes
		task.QuotaReplaceableBytes = replaceableBytes
		task.QuotaReplaceableSlots = replaceableSlots
		turn, err := s.tasks.IsQuotaTurnAt(ctx, task.ID, "distribution_quota", markQuotaWaitPosition(task, "distribution_quota"))
		if err != nil {
			return false, temporary("task_state", err)
		}
		if !turn {
			return false, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
		}
	}
	var admissionErr error
	if reopenAnchored {
		admissionErr = s.gate.ReopenAnchoredProtection(ctx, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	} else if len(oldIDs) > 0 {
		admissionErr = s.gate.ReplaceProtectionSet(ctx, oldIDs, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	} else {
		admissionErr = s.gate.AdmitProtection(ctx, manifest.sessionID, manifest.scopeID, task.ID, manifest.scopeBytes, maxSessions, maxBytes)
	}
	if admissionErr != nil {
		if errors.Is(admissionErr, ErrProtectionAnchored) {
			return false, nil
		}
		if errors.Is(admissionErr, ErrProtectionCapacity) {
			task.QuotaWaitBytes = manifest.scopeBytes
			task.QuotaReplaceableBytes = replaceableBytes
			task.QuotaReplaceableSlots = replaceableSlots
			return false, tasks.WaitFor(tasks.WaitingQuota, "distribution_quota")
		}
		if errors.Is(admissionErr, ErrWritesClosed) || errors.Is(admissionErr, ErrRecoveryBarrier) {
			return false, tasks.WaitFor(tasks.WaitingDeps, "gc_admission")
		}
		if errors.Is(admissionErr, ErrProtectionBusy) {
			return false, tasks.WaitFor(tasks.WaitingDeps, "protection_transfer")
		}
		return false, temporary("protection_admission", admissionErr)
	}
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return false, temporary("task_state", err)
		}
		_, transferred := oldSessions[intent.ProtectionSessionID]
		if intent.ProtectionSessionID == "" || transferred {
			if err := s.service.records.SetBlobIntentProtectionSession(ctx, intent.ID, manifest.sessionID); err != nil {
				return false, temporary("task_state", err)
			}
			intent.ProtectionSessionID = manifest.sessionID
		}
		if intent.ProtectionSessionID == manifest.sessionID {
			err := s.tasks.ReconcileProtectionState(ctx, intent.ID, manifest.scopeID, []string{manifest.sessionID}, manifest.scopeBytes, manifest.scopeBytes)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, temporary("task_state", err)
			}
		}
	}
	if err := s.syncRootProtectionTask(ctx, task, taskLease, manifest.sessionID, requestedBytes); err != nil {
		return false, err
	}
	clearQuotaWaitHints(task)
	return true, nil
}

func clearQuotaWaitHints(task *tasks.Task) {
	task.QuotaWaitAt = time.Time{}
	task.QuotaWaitReason = ""
	task.QuotaWaitBytes = 0
	task.QuotaReplaceableBytes = 0
	task.QuotaReplaceableSlots = 0
}

func (s *solidifier) syncRootProtectionTask(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string, requestedBytes int64) error {
	latest, err := s.tasks.Get(ctx, task.ID)
	if err != nil {
		return temporary("task_state", err)
	}
	if latest.CancelRequested || latest.FencingToken != taskLease.FencingToken {
		return tasks.ErrCancelled
	}
	contains := false
	for _, id := range latest.SessionIDs {
		if id == sessionID {
			contains = true
			break
		}
	}
	if !contains {
		latest.SessionIDs = append(latest.SessionIDs, sessionID)
	}
	var reserved int64
	seen := make(map[string]bool, len(latest.SessionIDs))
	for _, id := range latest.SessionIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		session, active, err := s.gate.protectionSession(ctx, id)
		if err != nil {
			return temporary("protection_state", err)
		}
		if active {
			sessionBytes, err := taskProtectionReservationBytes(&latest, id, session)
			if err != nil {
				return temporary("protection_state", err)
			}
			if sessionBytes > math.MaxInt64-reserved {
				return temporary("protection_state", errors.New("task protection reservation overflows"))
			}
			reserved += sessionBytes
		}
	}
	latest.ScopeID = task.ScopeID
	latest.RequestedBytes = requestedBytes
	latest.ReservedBytes = reserved
	latest.GCProtected = len(latest.SessionIDs) > 0
	if err := s.tasks.SaveProgress(ctx, &latest, taskLease); err != nil {
		return temporary("task_state", err)
	}
	*task = latest
	return nil
}

func (s *solidifier) recordStagedManifestObservation(ctx context.Context, task *tasks.Task, manifest contentRef) error {
	operationID := fmt.Sprintf("%s-%s-%d", task.ID, objectLeaseKey(task.Authority, task.Repository, manifest.digest), s.gate.State().Epoch)
	if err := s.service.records.RecordLocalObservation(ctx, tasks.LocalObservation{
		Authority: task.Authority, Repository: task.Repository, Role: tasks.GraphManifest,
		Digest: manifest.digest, ActualSize: manifest.size, Generation: task.Generation,
		GCEpoch: s.gate.State().Epoch, OperationID: operationID, VerificationBasis: "distribution_readback",
	}); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func (s *solidifier) markStagedManifestPublished(ctx context.Context, task *tasks.Task, manifest *stagedManifest) error {
	if err := s.saveManifestGraphNode(ctx, task, manifest.ref, true, "published_unanchored"); err != nil {
		return err
	}
	return nil
}

func (s *solidifier) prepareStagedBlob(ctx context.Context, task *tasks.Task, manifest *stagedManifest, blob contentRef) (bool, tasks.BlobIntent, error) {
	cached, err := s.cache.GetAnyRetained(ctx, task.Authority, task.Repository, blob.digest)
	if errors.Is(err, storage.ErrNotFound) {
		return false, tasks.BlobIntent{}, nil
	}
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("storage", err)
	}
	defer cached.Body.Close()
	if cached.Size != blob.size {
		return false, tasks.BlobIntent{}, permanent("manifest_invalid", errors.New("cached blob size differs from its descriptor"))
	}
	generation, err := s.blobContentGeneration(ctx, task, blob.digest)
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("generation", err)
	}
	intent, _, err := s.service.records.EnsureBlobIntent(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest}, generation, "")
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	demandID := stagedDemandID(task.ID, manifest.scopeID)
	if err := s.service.records.LinkBlobDemandScope(ctx, intent.ID, demandID, task.ID, manifest.scopeID, manifest.sessionID, manifest.scopeBytes); err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	ready, err := s.service.records.SetBlobIntentReady(ctx, intent.ID, cached.MediaType, cached.DataVersion, cached.Size)
	if err != nil {
		return false, tasks.BlobIntent{}, temporary("task_state", err)
	}
	if ready.ID != intent.ID {
		if err := s.service.records.LinkBlobDemandScope(ctx, ready.ID, demandID, task.ID, manifest.scopeID, manifest.sessionID, manifest.scopeBytes); err != nil {
			return false, tasks.BlobIntent{}, temporary("task_state", err)
		}
	}
	return true, ready, nil
}

func (s *solidifier) anchorManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, manifest *stagedManifest) error {
	session, active, err := s.gate.protectionSession(ctx, manifest.sessionID)
	if err != nil {
		return temporary("protection_state", err)
	}
	if active {
		if session.PendingOperations != 0 || session.UncertainRemoteResult {
			return tasks.WaitFor(tasks.WaitingDeps, "remote_operations_pending")
		}
	}
	if err := s.saveManifestGraphNode(ctx, task, manifest.ref, true, "anchored"); err != nil {
		return err
	}
	intentIDs, err := s.markManifestBlobIntentsAnchored(ctx, task, manifest)
	if err != nil {
		return err
	}
	if active {
		if err := s.gate.MarkProtectionAnchored(ctx, manifest.sessionID); err != nil {
			return temporary("protection_anchor", err)
		}
	}
	if err := s.removeRootProtectionSession(ctx, task, taskLease, manifest.sessionID); err != nil {
		return err
	}
	if active {
		if err := s.gate.ReleaseProtection(ctx, manifest.sessionID, "anchored"); err != nil {
			return temporary("protection_release", err)
		}
	}
	return s.anchorBlobTasks(ctx, intentIDs)
}

func (s *solidifier) finalizeAnchoredManifestBlobs(ctx context.Context, task *tasks.Task, manifest *stagedManifest) error {
	intentIDs, err := s.markManifestBlobIntentsAnchored(ctx, task, manifest)
	if err != nil {
		return err
	}
	return s.anchorBlobTasks(ctx, intentIDs)
}

func (s *solidifier) markManifestBlobIntentsAnchored(ctx context.Context, task *tasks.Task, manifest *stagedManifest) ([]string, error) {
	intentIDs := make([]string, 0, len(manifest.blobs))
	for _, blob := range manifest.blobs {
		intent, err := s.service.records.GetBlobIntentByIdentity(ctx, tasks.BlobIdentity{Authority: task.Authority, Repository: task.Repository, Digest: blob.digest})
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return nil, temporary("task_state", err)
		}
		if _, err := s.service.records.SetBlobIntentState(ctx, intent.ID, tasks.IntentAnchored, "", ""); err != nil {
			return nil, temporary("task_state", err)
		}
		s.service.wakeBlobDemandRoots(ctx, intent.ID)
		intentIDs = append(intentIDs, intent.ID)
	}
	return intentIDs, nil
}

func (s *solidifier) anchorBlobTasks(ctx context.Context, intentIDs []string) error {
	for _, intentID := range intentIDs {
		if err := s.tasks.AnchorBlobTask(ctx, intentID); err != nil && !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, tasks.ErrCancelled) {
			return temporary("task_state", err)
		}
	}
	return nil
}

func (s *solidifier) blobContentGeneration(ctx context.Context, task *tasks.Task, digest string) (uint64, error) {
	current, err := s.service.generation(ctx, task.Authority, task.Repository, digest)
	if err != nil {
		return 0, err
	}
	return current.Generation, nil
}

func (s *solidifier) releaseEmptyManifestScope(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string) error {
	if err := s.gate.ReleaseEmptyProtection(ctx, sessionID, task.ID); err != nil {
		if errors.Is(err, ErrProtectionBusy) || errors.Is(err, ErrProtectionHasContent) {
			return nil
		}
		return temporary("protection_release", err)
	}
	return s.removeRootProtectionSession(ctx, task, taskLease, sessionID)
}

func (s *solidifier) removeRootProtectionSession(ctx context.Context, task *tasks.Task, taskLease lease.Lease, sessionID string) error {
	filtered := task.SessionIDs[:0]
	for _, id := range task.SessionIDs {
		if id != sessionID {
			filtered = append(filtered, id)
		}
	}
	task.SessionIDs = filtered
	if len(task.SessionIDs) == 0 {
		task.GCProtected = false
		task.ReservedBytes = 0
	} else {
		task.GCProtected = true
		var reserved int64
		for _, id := range task.SessionIDs {
			session, active, err := s.gate.protectionSession(ctx, id)
			if err != nil {
				return temporary("protection_state", err)
			}
			if active {
				sessionBytes, err := taskProtectionReservationBytes(task, id, session)
				if err != nil {
					return temporary("protection_state", err)
				}
				if sessionBytes > math.MaxInt64-reserved {
					return temporary("protection_state", errors.New("task protection reservation overflows"))
				}
				reserved += sessionBytes
			}
		}
		task.ReservedBytes = reserved
	}
	if err := s.tasks.SaveProgress(ctx, task, taskLease); err != nil {
		return temporary("task_state", err)
	}
	return nil
}

func stagedSessionID(scopeID string) string {
	return hashID("scope-session", scopeID)
}

func stagedDemandID(rootTaskID, scopeID string) string {
	return hashID("scope-demand", rootTaskID, scopeID)
}

func rootTaskHasProtection(task *tasks.Task, sessionID string) bool {
	for _, id := range task.SessionIDs {
		if id == sessionID {
			return true
		}
	}
	return false
}
