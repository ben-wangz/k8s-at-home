package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
)

type localObservationIndex struct {
	SchemaVersion  int       `json:"schemaVersion"`
	Authority      string    `json:"authority"`
	Repository     string    `json:"repository"`
	Role           GraphRole `json:"role"`
	Digest         string    `json:"digest"`
	ActualSize     int64     `json:"actualSize"`
	Generation     uint64    `json:"generation"`
	GCEpoch        uint64    `json:"gcEpoch"`
	ObservationKey string    `json:"observationKey"`
}

func (r *Records) SaveGraphNode(ctx context.Context, node *GraphNode) error {
	if node == nil || !validID(node.RootTaskID) || !validGraphRole(node.Role) || node.Digest == "" || node.MediaType == "" || node.DeclaredSize < 0 || node.ActualSize < 0 {
		return errors.New("graph node is invalid")
	}
	key, err := graphNodeKey(node.RootTaskID, node.Role, node.Digest)
	if err != nil {
		return err
	}
	node.SchemaVersion = solidificationRecordSchema
	data, err := json.Marshal(node)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: true}
	if node.version != "" {
		options = storage.PutOptions{IfMatch: node.version}
	}
	entry, err := r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		existing, getErr := r.GetGraphNode(ctx, node.RootTaskID, node.Role, node.Digest)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		existingData, marshalErr := json.Marshal(existing)
		if marshalErr != nil {
			return marshalErr
		}
		if bytes.Equal(existingData, data) {
			node.version = existing.version
			return nil
		}
		entry, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: existing.version})
	}
	if err != nil {
		return err
	}
	node.version = entry.Version
	return nil
}

func (r *Records) GetGraphNode(ctx context.Context, rootTaskID string, role GraphRole, digest string) (GraphNode, error) {
	key, err := graphNodeKey(rootTaskID, role, digest)
	if err != nil {
		return GraphNode{}, err
	}
	object, err := r.store.Get(ctx, key)
	if err != nil {
		return GraphNode{}, err
	}
	var node GraphNode
	if err := decodeRecord(object, 4<<20, &node); err != nil {
		return GraphNode{}, err
	}
	if node.SchemaVersion != solidificationRecordSchema || node.RootTaskID != rootTaskID || node.Role != role || node.Digest != digest || node.MediaType == "" || node.DeclaredSize < 0 || node.ActualSize < 0 {
		return GraphNode{}, errors.New("graph node record is invalid")
	}
	node.version = object.Version
	return node, nil
}

func (r *Records) ListGraphNodes(ctx context.Context, rootTaskID, token string, limit int) ([]GraphNode, string, error) {
	if !validID(rootTaskID) {
		return nil, "", errors.New("root task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := r.store.List(ctx, "solidification/graphs/"+rootTaskID+"/nodes/", token, limit)
	if err != nil {
		return nil, "", err
	}
	nodes := make([]GraphNode, 0, len(entries))
	for _, entry := range entries {
		object, getErr := r.store.Get(ctx, entry.Key)
		if getErr != nil {
			return nil, "", getErr
		}
		var node GraphNode
		if decodeErr := decodeRecord(object, 4<<20, &node); decodeErr != nil {
			return nil, "", decodeErr
		}
		if node.SchemaVersion != solidificationRecordSchema || node.RootTaskID != rootTaskID || !validGraphRole(node.Role) || node.Digest == "" || node.MediaType == "" || node.DeclaredSize < 0 || node.ActualSize < 0 {
			return nil, "", errors.New("graph node record is invalid")
		}
		node.version = object.Version
		nodes = append(nodes, node)
	}
	return nodes, next, nil
}

func (r *Records) SaveDependencyEdge(ctx context.Context, edge DependencyEdge) error {
	if !validID(edge.RootTaskID) || !validGraphRole(edge.ParentRole) || !validGraphRole(edge.ChildRole) || edge.ParentDigest == "" || edge.ChildDigest == "" || edge.Order < 0 || edge.Descriptor.Digest != edge.ChildDigest || edge.Descriptor.Size < 0 || edge.Descriptor.MediaType == "" {
		return errors.New("dependency edge is invalid")
	}
	key, err := dependencyEdgeKey(edge)
	if err != nil {
		return err
	}
	edge.SchemaVersion = solidificationRecordSchema
	data, err := json.Marshal(edge)
	if err != nil {
		return err
	}
	_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		object, getErr := r.store.Get(ctx, key)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		var existing DependencyEdge
		if decodeErr := decodeRecord(object, 4<<20, &existing); decodeErr != nil {
			return decodeErr
		}
		existingData, marshalErr := json.Marshal(existing)
		if marshalErr != nil {
			return marshalErr
		}
		if bytes.Equal(existingData, data) {
			return nil
		}
		return storage.ErrConditionFailed
	}
	return err
}

func (r *Records) ListDependencyEdges(ctx context.Context, rootTaskID, token string, limit int) ([]DependencyEdge, string, error) {
	if !validID(rootTaskID) {
		return nil, "", errors.New("root task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	entries, next, err := r.store.List(ctx, "solidification/graphs/"+rootTaskID+"/edges/", token, limit)
	if err != nil {
		return nil, "", err
	}
	edges := make([]DependencyEdge, 0, len(entries))
	for _, entry := range entries {
		object, getErr := r.store.Get(ctx, entry.Key)
		if getErr != nil {
			return nil, "", getErr
		}
		var edge DependencyEdge
		if decodeErr := decodeRecord(object, 4<<20, &edge); decodeErr != nil {
			return nil, "", decodeErr
		}
		if edge.SchemaVersion != solidificationRecordSchema || edge.RootTaskID != rootTaskID || !validGraphRole(edge.ParentRole) || !validGraphRole(edge.ChildRole) || edge.ParentDigest == "" || edge.ChildDigest == "" || edge.Order < 0 || edge.Descriptor.Digest != edge.ChildDigest || edge.Descriptor.MediaType == "" || edge.Descriptor.Size < 0 {
			return nil, "", errors.New("dependency edge record is invalid")
		}
		edges = append(edges, edge)
	}
	return edges, next, nil
}
