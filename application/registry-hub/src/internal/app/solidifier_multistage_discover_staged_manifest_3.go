package app

import (
	"bytes"
	"context"
	"errors"
	"math"
	"registry-hub/internal/artifact"
	"registry-hub/internal/config"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

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
