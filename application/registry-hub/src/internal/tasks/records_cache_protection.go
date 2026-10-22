package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
)

const cacheProtectionRecordSchema = 1

type CacheProtectionReference struct {
	SchemaVersion int    `json:"schemaVersion"`
	TaskID        string `json:"taskID"`
	Authority     string `json:"authority"`
	Repository    string `json:"repository"`
	Digest        string `json:"digest"`
}

func (r *Records) EnsureCacheProtectionReference(ctx context.Context, taskID, authority, repository, digest string) error {
	if !validID(taskID) || authority == "" || repository == "" || digest == "" {
		return errors.New("cache protection reference is invalid")
	}
	reference := CacheProtectionReference{
		SchemaVersion: cacheProtectionRecordSchema,
		TaskID:        taskID,
		Authority:     authority,
		Repository:    repository,
		Digest:        digest,
	}
	digestHash, err := recordHash(digest)
	if err != nil {
		return err
	}
	taskIndexKey := cacheProtectionTaskIndexKey(taskID, digestHash)
	digestIndexKey := cacheProtectionDigestIndexKey(digestHash, taskID)
	if err := r.putCacheProtectionReference(ctx, taskIndexKey, reference); err != nil {
		return err
	}
	return r.putCacheProtectionReference(ctx, digestIndexKey, reference)
}

func (r *Records) putCacheProtectionReference(ctx context.Context, key string, reference CacheProtectionReference) error {
	data, err := json.Marshal(reference)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 16; attempt++ {
		_, headErr := r.store.Head(ctx, key)
		options := storage.PutOptions{IfAbsent: errors.Is(headErr, storage.ErrNotFound)}
		if headErr == nil {
			object, getErr := r.store.Get(ctx, key)
			if getErr != nil {
				return getErr
			}
			var current CacheProtectionReference
			if err := decodeRecord(object, 1<<20, &current); err != nil {
				return err
			}
			if current != reference {
				return errors.New("cache protection index conflicts with its identity")
			}
			return nil
		}
		if !errors.Is(headErr, storage.ErrNotFound) {
			return headErr
		}
		_, err = r.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			continue
		}
		if errors.Is(err, storage.ErrUnknownCommit) {
			object, getErr := r.store.Get(ctx, key)
			if getErr == nil {
				var current CacheProtectionReference
				if decodeErr := decodeRecord(object, 1<<20, &current); decodeErr != nil {
					return decodeErr
				}
				if current == reference {
					return nil
				}
			}
			continue
		}
		return err
	}
	return storage.ErrUnavailable
}

func (r *Records) ListCacheProtectionReferences(ctx context.Context, digest, token string, limit int) ([]CacheProtectionReference, string, error) {
	if digest == "" {
		return nil, "", errors.New("cache protection digest is empty")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	digestHash, err := recordHash(digest)
	if err != nil {
		return nil, "", err
	}
	entries, next, err := r.store.List(ctx, "solidification/cache-protection/digests/"+digestHash+"/", token, limit)
	if err != nil {
		return nil, "", err
	}
	result := make([]CacheProtectionReference, 0, len(entries))
	for _, entry := range entries {
		object, err := r.store.Get(ctx, entry.Key)
		if err != nil {
			return nil, "", err
		}
		var reference CacheProtectionReference
		if err := decodeRecord(object, 1<<20, &reference); err != nil {
			return nil, "", err
		}
		if reference.SchemaVersion != cacheProtectionRecordSchema || !validID(reference.TaskID) || reference.Digest != digest || reference.Authority == "" || reference.Repository == "" {
			return nil, "", errors.New("cache protection index record is invalid")
		}
		result = append(result, reference)
	}
	return result, next, nil
}

func (r *Records) DeleteCacheProtectionReferences(ctx context.Context, taskID string, limit int) (int, bool, error) {
	if !validID(taskID) {
		return 0, false, errors.New("cache protection task ID is invalid")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	prefix := "solidification/cache-protection/tasks/" + taskID + "/"
	entries, _, err := r.store.List(ctx, prefix, "", limit)
	if err != nil {
		return 0, false, err
	}
	removed := 0
	for _, entry := range entries {
		object, err := r.store.Get(ctx, entry.Key)
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return removed, false, err
		}
		var reference CacheProtectionReference
		if err := decodeRecord(object, 1<<20, &reference); err != nil {
			return removed, false, err
		}
		if reference.SchemaVersion != cacheProtectionRecordSchema || reference.TaskID != taskID || reference.Digest == "" {
			return removed, false, errors.New("cache protection task index record is invalid")
		}
		digestHash, err := recordHash(reference.Digest)
		if err != nil {
			return removed, false, err
		}
		digestKey := cacheProtectionDigestIndexKey(digestHash, taskID)
		if digestEntry, err := r.store.Head(ctx, digestKey); err == nil {
			if err := r.store.DeleteIfMatch(ctx, digestKey, digestEntry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
				return removed, false, err
			}
		} else if !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
		if err := r.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			return removed, false, err
		}
		removed++
	}
	remaining, _, err := r.store.List(ctx, prefix, "", 1)
	if err != nil {
		return removed, false, err
	}
	return removed, len(remaining) == 0, nil
}

func cacheProtectionTaskIndexKey(taskID, digestHash string) string {
	return "solidification/cache-protection/tasks/" + taskID + "/" + digestHash + ".json"
}

func cacheProtectionDigestIndexKey(digestHash, taskID string) string {
	return "solidification/cache-protection/digests/" + digestHash + "/" + taskID + ".json"
}
