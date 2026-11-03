package tasks

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (r *Records) RepairRootBlobDemandIndex(ctx context.Context, demand BlobDemand) error {
	return r.saveRootBlobDemandIndex(ctx, demand)
}

func (r *Records) ReadRecoveryBlobDemand(ctx context.Context, key, intentID string) (BlobDemand, error) {
	demand, _, err := r.readBlobDemand(ctx, key)
	if err == nil && (demand.IntentID != intentID || demand.ScopeID == "" && (demand.RootTaskID != "" || demand.SessionID != "" || demand.ReservedBytes != 0) || demand.ScopeID != "" && !validID(demand.SessionID)) {
		err = storage.ErrCorruptObject
	}
	return demand, err
}

func (r *Records) RepairCacheProtectionHalf(ctx context.Context, taskID, authority, repository, digest string, half int) error {
	if !validID(taskID) || authority == "" || repository == "" || digest == "" || half < 0 || half > 1 {
		return errors.New("cache protection reference is invalid")
	}
	hash, err := recordHash(digest)
	if err != nil {
		return err
	}
	key := cacheProtectionTaskIndexKey(taskID, hash)
	if half == 1 {
		key = cacheProtectionDigestIndexKey(hash, taskID)
	}
	return r.putCacheProtectionReference(ctx, key, CacheProtectionReference{SchemaVersion: cacheProtectionRecordSchema, TaskID: taskID, Authority: authority, Repository: repository, Digest: digest})
}
