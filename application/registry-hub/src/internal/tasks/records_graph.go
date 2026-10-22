package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"strings"
	"time"
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

func (r *Records) RecordLocalObservation(ctx context.Context, observation LocalObservation) error {
	if observation.Authority == "" || observation.Repository == "" || !validGraphRole(observation.Role) || observation.Digest == "" || observation.ActualSize < 0 || observation.OperationID == "" || observation.VerificationBasis == "" {
		return errors.New("local observation is invalid")
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}
	observation.SchemaVersion = solidificationRecordSchema
	key, err := localObservationRecordKey(observation)
	if err != nil {
		return err
	}
	data, err := json.Marshal(observation)
	if err != nil {
		return err
	}
	_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) || errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrUnknownCommit) {
		object, getErr := r.store.Get(ctx, key)
		if getErr != nil {
			return errors.Join(err, getErr)
		}
		var existing LocalObservation
		if decodeErr := decodeRecord(object, 1<<20, &existing); decodeErr != nil {
			return decodeErr
		}
		if !sameLocalObservationIdentity(existing, observation) || existing.ActualSize != observation.ActualSize || existing.Generation != observation.Generation || existing.GCEpoch != observation.GCEpoch || existing.VerificationBasis != observation.VerificationBasis {
			return storage.ErrConditionFailed
		}
		observation = existing
	} else if err != nil {
		return err
	}
	return r.updateLocalObservationIndex(ctx, observation, key)
}

func (r *Records) FindLocalObservation(ctx context.Context, authority, repository string, role GraphRole, digest string, actualSize int64, generation, gcEpoch uint64) (LocalObservation, error) {
	if authority == "" || repository == "" || !validGraphRole(role) || digest == "" || actualSize < 0 || gcEpoch == 0 {
		return LocalObservation{}, errors.New("local observation query is invalid")
	}
	indexKey, err := localObservationIndexKey(authority, repository, role, digest)
	if err != nil {
		return LocalObservation{}, err
	}
	indexObject, err := r.store.Get(ctx, indexKey)
	if err != nil {
		return LocalObservation{}, err
	}
	var index localObservationIndex
	if err := decodeRecord(indexObject, 1<<20, &index); err != nil {
		return LocalObservation{}, errors.Join(storage.ErrCorruptObject, err)
	}
	if index.SchemaVersion != solidificationRecordSchema || index.Authority != authority || index.Repository != repository || index.Role != role || index.Digest != digest || index.ActualSize != actualSize || index.Generation != generation || index.GCEpoch != gcEpoch {
		return LocalObservation{}, storage.ErrNotFound
	}
	if !strings.HasPrefix(index.ObservationKey, "solidification/observations/") || !strings.HasSuffix(index.ObservationKey, ".json") {
		return LocalObservation{}, storage.ErrCorruptObject
	}
	observationObject, err := r.store.Get(ctx, index.ObservationKey)
	if err != nil {
		return LocalObservation{}, err
	}
	var observation LocalObservation
	if err := decodeRecord(observationObject, 1<<20, &observation); err != nil {
		return LocalObservation{}, errors.Join(storage.ErrCorruptObject, err)
	}
	if observation.SchemaVersion != solidificationRecordSchema || observation.Authority != authority || observation.Repository != repository || observation.Role != role || observation.Digest != digest || observation.ActualSize != actualSize || observation.Generation != generation || observation.GCEpoch != gcEpoch || observation.VerificationBasis != "distribution_readback" || observation.OperationID == "" || observation.ObservedAt.IsZero() {
		return LocalObservation{}, storage.ErrCorruptObject
	}
	return observation, nil
}

func (r *Records) updateLocalObservationIndex(ctx context.Context, observation LocalObservation, observationKey string) error {
	key, err := localObservationIndexKey(observation.Authority, observation.Repository, observation.Role, observation.Digest)
	if err != nil {
		return err
	}
	index := localObservationIndex{
		SchemaVersion: solidificationRecordSchema, Authority: observation.Authority, Repository: observation.Repository,
		Role: observation.Role, Digest: observation.Digest, ActualSize: observation.ActualSize,
		Generation: observation.Generation, GCEpoch: observation.GCEpoch, ObservationKey: observationKey,
	}
	data, err := json.Marshal(index)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		current, headErr := r.store.Head(ctx, key)
		if errors.Is(headErr, storage.ErrNotFound) {
			_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
		} else if headErr != nil {
			return headErr
		} else {
			_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: current.Version})
		}
		if err == nil {
			return nil
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			object, readErr := r.store.Get(ctx, key)
			if readErr == nil {
				var latest localObservationIndex
				if decodeErr := decodeRecord(object, 1<<20, &latest); decodeErr == nil && sameLocalObservationIndex(latest, index) {
					return nil
				}
			}
			if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
				return errors.Join(err, readErr)
			}
		}
		if !errors.Is(err, storage.ErrConditionFailed) && !errors.Is(err, storage.ErrAlreadyExists) && !errors.Is(err, storage.ErrUnknownCommit) {
			return err
		}
	}
	return storage.ErrUnavailable
}

func localObservationRecordKey(observation LocalObservation) (string, error) {
	keyHash, err := recordHash(struct {
		Authority   string
		Repository  string
		Role        GraphRole
		Digest      string
		OperationID string
	}{observation.Authority, observation.Repository, observation.Role, observation.Digest, observation.OperationID})
	if err != nil {
		return "", err
	}
	return "solidification/observations/" + keyHash + ".json", nil
}

func localObservationIndexKey(authority, repository string, role GraphRole, digest string) (string, error) {
	keyHash, err := recordHash(struct {
		Authority  string
		Repository string
		Role       GraphRole
		Digest     string
	}{authority, repository, role, digest})
	if err != nil {
		return "", err
	}
	return "solidification/observation-index/" + keyHash + ".json", nil
}

func sameLocalObservationIdentity(first, second LocalObservation) bool {
	return first.SchemaVersion == solidificationRecordSchema && first.Authority == second.Authority && first.Repository == second.Repository && first.Role == second.Role && first.Digest == second.Digest && first.OperationID == second.OperationID
}

func sameLocalObservationIndex(first, second localObservationIndex) bool {
	return first.SchemaVersion == second.SchemaVersion && first.Authority == second.Authority && first.Repository == second.Repository && first.Role == second.Role && first.Digest == second.Digest && first.ActualSize == second.ActualSize && first.Generation == second.Generation && first.GCEpoch == second.GCEpoch && first.ObservationKey == second.ObservationKey
}

func graphNodeKey(rootTaskID string, role GraphRole, digest string) (string, error) {
	if !validID(rootTaskID) || !validGraphRole(role) || digest == "" || len(digest) > 512 {
		return "", errors.New("graph node identity is invalid")
	}
	hash, err := recordHash(struct {
		Role   GraphRole
		Digest string
	}{role, digest})
	if err != nil {
		return "", err
	}
	return "solidification/graphs/" + rootTaskID + "/nodes/" + hash + ".json", nil
}

func dependencyEdgeKey(edge DependencyEdge) (string, error) {
	hash, err := recordHash(struct {
		RootTaskID   string
		ParentRole   GraphRole
		ParentDigest string
		Order        int
	}{edge.RootTaskID, edge.ParentRole, edge.ParentDigest, edge.Order})
	if err != nil {
		return "", err
	}
	return "solidification/graphs/" + edge.RootTaskID + "/edges/" + hash + ".json", nil
}
