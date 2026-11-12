package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
)

const discoverySnapshotSchema = 1

type discoveryManifest struct {
	Reference tasks.GraphEntry   `json:"reference"`
	Blobs     []tasks.GraphEntry `json:"blobs"`
	Children  []tasks.GraphEntry `json:"children"`
}

type discoverySnapshot struct {
	SchemaVersion      int                    `json:"schemaVersion"`
	TaskID             string                 `json:"taskID"`
	RoundID            string                 `json:"roundID"`
	Authority          string                 `json:"authority"`
	Repository         string                 `json:"repository"`
	RootDigest         string                 `json:"rootDigest"`
	Generation         uint64                 `json:"generation"`
	Compatibility      string                 `json:"compatibility"`
	Revision           string                 `json:"revision,omitempty"`
	Manifests          []discoveryManifest    `json:"manifests"`
	Edges              []tasks.DependencyEdge `json:"edges"`
	Parents            map[string][]string    `json:"parents"`
	ContentGenerations map[string]uint64      `json:"contentGenerations"`
}

func discoverySnapshotKey(taskID string) string {
	return "solidification/graphs/" + taskID + "/discovery.json"
}

func (s *solidifier) discoveryCompatibility() string {
	l := s.cfg.Limits
	data, _ := json.Marshal([]int64{discoverySnapshotSchema, l.ManifestBytes, l.ObjectBytes, int64(l.ClosureObjects), l.ClosureBytes, int64(l.ClosureDepth), int64(l.MaxDescriptors), l.DescriptorDataBytes})
	return artifact.CalculateSHA256(data)
}

func (s *solidifier) loadOrDiscoverGraph(ctx context.Context, task *tasks.Task, holder lease.Lease) (*stagedGraph, error) {
	snapshot, _, err := inventory.Read[discoverySnapshot](ctx, s.store, discoverySnapshotKey(task.ID))
	if err == nil && s.compatibleDiscovery(task, snapshot) {
		graph, valid, err := s.restoreDiscoveryGraph(ctx, task, snapshot)
		if err != nil {
			return nil, err
		}
		if valid {
			graph.reused = true
			graph.revision = snapshot.Revision
			graph.parents = snapshot.Parents
			return graph, nil
		}
	} else if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
		return nil, err
	}
	graph := &stagedGraph{byManifest: make(map[string]*stagedManifest), blobs: make(map[string]contentRef), active: make(map[string]bool)}
	root, err := s.discoverStagedManifest(ctx, task, holder, graph, contentRef{digest: task.RootDigest, mediaType: task.MediaType, size: -1}, 0, true)
	if err != nil {
		return nil, err
	}
	graph.root = root.ref
	if err := s.saveDiscoverySnapshot(ctx, task, holder, graph); err != nil {
		return nil, err
	}
	return graph, nil
}

func (s *solidifier) compatibleDiscovery(task *tasks.Task, snapshot discoverySnapshot) bool {
	if snapshot.SchemaVersion != discoverySnapshotSchema || snapshot.TaskID != task.ID || snapshot.RoundID != task.RoundID || snapshot.Authority != task.Authority || snapshot.Repository != task.Repository || snapshot.RootDigest != task.RootDigest || snapshot.Generation != task.Generation || snapshot.Compatibility != s.discoveryCompatibility() || len(snapshot.Manifests) == 0 || len(snapshot.Manifests) > s.cfg.Limits.ClosureObjects {
		return false
	}
	revision := snapshot.Revision
	snapshot.Revision = ""
	data, err := json.Marshal(snapshot)
	return err == nil && revision == artifact.CalculateSHA256(data)
}

func (s *solidifier) saveDiscoverySnapshot(ctx context.Context, task *tasks.Task, holder lease.Lease, graph *stagedGraph) error {
	snapshot := discoverySnapshot{SchemaVersion: discoverySnapshotSchema, TaskID: task.ID, RoundID: task.RoundID, Authority: task.Authority, Repository: task.Repository, RootDigest: task.RootDigest, Generation: task.Generation, Compatibility: s.discoveryCompatibility(), Parents: make(map[string][]string), ContentGenerations: make(map[string]uint64)}
	for _, manifest := range graph.manifests {
		entry := discoveryManifest{Reference: graphEntry(manifest.ref)}
		for _, blob := range manifest.blobs {
			entry.Blobs = append(entry.Blobs, graphEntry(blob))
			snapshot.Parents[blob.digest] = append(snapshot.Parents[blob.digest], manifest.ref.digest)
		}
		for _, child := range manifest.children {
			entry.Children = append(entry.Children, graphEntry(child))
			snapshot.Parents[child.digest] = append(snapshot.Parents[child.digest], manifest.ref.digest)
		}
		snapshot.Manifests = append(snapshot.Manifests, entry)
	}
	nodes, err := s.discoveryNodes(ctx, task.ID)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if !node.DiscoveryComplete || node.ContentGeneration == nil {
			return storage.ErrCorruptObject
		}
		snapshot.ContentGenerations[node.Digest] = *node.ContentGeneration
	}
	for token := ""; ; {
		edges, next, err := s.service.records.ListDependencyEdges(ctx, task.ID, token, 500)
		if err != nil {
			return err
		}
		snapshot.Edges = append(snapshot.Edges, edges...)
		if next == "" {
			break
		}
		token = next
	}
	data, err := json.Marshal(snapshot)
	if err != nil || len(data) > 7<<20 {
		return err
	}
	snapshot.Revision = artifact.CalculateSHA256(data)
	if err := s.tasks.Leases().Validate(ctx, holder); err != nil {
		return err
	}
	if err := s.service.checkTaskGeneration(ctx, task); err != nil {
		return err
	}
	err = saveGraphRecord(s.graphWriteContext(ctx, task, holder), s.store, discoverySnapshotKey(task.ID), snapshot)
	if err == nil {
		graph.revision, graph.parents = snapshot.Revision, snapshot.Parents
	}
	return err
}

func (s *solidifier) discoveryNodes(ctx context.Context, id string) ([]tasks.GraphNode, error) {
	var nodes []tasks.GraphNode
	for token := ""; ; {
		page, next, err := s.service.records.ListGraphNodes(ctx, id, token, 500)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, page...)
		if next == "" {
			return nodes, nil
		}
		token = next
	}
}

func sameDiscoveryEdges(left, right []tasks.DependencyEdge) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return bytes.Equal(a, b)
}
