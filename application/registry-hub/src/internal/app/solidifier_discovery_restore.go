package app

import (
	"context"
	"reflect"
	"registry-hub/internal/tasks"
)

func snapshotRef(entry tasks.GraphEntry) contentRef {
	return contentRef{digest: entry.Digest, mediaType: entry.MediaType, size: entry.Size, artifactType: entry.ArtifactType, annotations: cloneAnnotations(entry.Annotations), platform: append([]byte(nil), entry.Platform...), urls: append([]string(nil), entry.URLs...)}
}

func (s *solidifier) restoreDiscoveryGraph(ctx context.Context, task *tasks.Task, snapshot discoverySnapshot) (*stagedGraph, bool, error) {
	valid, err := s.discoveryEdgesCurrent(ctx, task, snapshot)
	if err != nil || !valid {
		return nil, false, err
	}
	graph, valid := s.decodeDiscoveryGraph(task, snapshot)
	if !valid {
		return nil, false, nil
	}
	for digest, expected := range snapshot.ContentGenerations {
		current, err := s.contentGeneration(ctx, task.Authority, task.Repository, digest)
		if err != nil {
			return nil, false, err
		}
		if current != expected {
			return nil, false, nil
		}
	}
	for _, blob := range graph.blobs {
		if _, ok := snapshot.ContentGenerations[blob.digest]; !ok {
			return nil, false, nil
		}
		if err := s.saveGraphNodeAtGeneration(ctx, task, blob, tasks.GraphBlob, true, "discovered", snapshot.ContentGenerations[blob.digest]); err != nil {
			return nil, false, err
		}
	}
	for _, manifest := range graph.manifests {
		if _, ok := snapshot.ContentGenerations[manifest.ref.digest]; !ok {
			return nil, false, nil
		}
		if err := s.saveGraphNodeAtGeneration(ctx, task, manifest.ref, tasks.GraphManifest, true, "discovered", snapshot.ContentGenerations[manifest.ref.digest]); err != nil {
			return nil, false, err
		}
	}
	return graph, true, nil
}

func (s *solidifier) discoveryEdgesCurrent(ctx context.Context, task *tasks.Task, snapshot discoverySnapshot) (bool, error) {
	var edges []tasks.DependencyEdge
	for token := ""; ; {
		page, next, err := s.service.records.ListDependencyEdges(ctx, task.ID, token, 500)
		if err != nil {
			return false, err
		}
		edges = append(edges, page...)
		if next == "" {
			break
		}
		token = next
	}
	if !sameDiscoveryEdges(edges, snapshot.Edges) {
		return false, nil
	}
	for _, edge := range edges {
		if len(edge.Descriptor.Data) == 0 {
			continue
		}
		cached, err := s.cache.InspectAny(ctx, task.Authority, task.Repository, edge.ChildDigest, edge.Descriptor.MediaType)
		if err != nil {
			return false, err
		}
		if !cached.Present {
			return false, nil
		}
	}
	return true, nil
}

func (s *solidifier) decodeDiscoveryGraph(task *tasks.Task, snapshot discoverySnapshot) (*stagedGraph, bool) {
	graph := &stagedGraph{byManifest: make(map[string]*stagedManifest), blobs: make(map[string]contentRef), active: make(map[string]bool)}
	parents := make(map[string][]string)
	depths := make(map[string]int)
	for _, entry := range snapshot.Manifests {
		reference := snapshotRef(entry.Reference)
		if artifactDigest(reference.digest) != nil || reference.size <= 0 || reference.mediaType == "" || reference.size > s.cfg.Limits.ManifestBytes || len(entry.Blobs)+len(entry.Children) > s.cfg.Limits.MaxDescriptors || graph.byManifest[reference.digest] != nil {
			return nil, false
		}
		if err := graph.add(reference, s.cfg.Limits); err != nil {
			return nil, false
		}
		manifest := &stagedManifest{ref: reference}
		for _, blobEntry := range entry.Blobs {
			blob := snapshotRef(blobEntry)
			if artifactDigest(blob.digest) != nil || blob.mediaType == "" || s.validateStagedBlob(graph, blob) != nil {
				return nil, false
			}
			manifest.blobs = append(manifest.blobs, blob)
			parents[blob.digest] = append(parents[blob.digest], reference.digest)
		}
		for _, childEntry := range entry.Children {
			child := snapshotRef(childEntry)
			known := graph.byManifest[child.digest]
			if known == nil || known.ref.size != child.size || !sameMediaType(known.ref.mediaType, child.mediaType) {
				return nil, false
			}
			depths[reference.digest] = max(depths[reference.digest], depths[child.digest]+1)
			manifest.children = append(manifest.children, child)
			parents[child.digest] = append(parents[child.digest], reference.digest)
		}
		if depths[reference.digest] > s.cfg.Limits.ClosureDepth {
			return nil, false
		}
		graph.manifests = append(graph.manifests, manifest)
		graph.byManifest[reference.digest] = manifest
	}
	root := graph.byManifest[task.RootDigest]
	if root == nil || !sameMediaType(root.ref.mediaType, task.MediaType) || !reflect.DeepEqual(parents, snapshot.Parents) {
		return nil, false
	}
	graph.root = root.ref
	return graph, true
}
