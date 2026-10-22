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
