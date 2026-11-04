package tasks

import (
	"context"
	"hash/fnv"
)

type demandLockContextKey struct{}

type heldDemandLock struct {
	records *Records
	index   uint32
	parent  *heldDemandLock
}

// Demand admission and the final unowned check share a lock in the single backend.
func (r *Records) WithBlobDemandLock(ctx context.Context, id string, run func(context.Context) error) error {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(id))
	index := hash.Sum32() % uint32(len(r.demandLocks))
	held, _ := ctx.Value(demandLockContextKey{}).(*heldDemandLock)
	for current := held; current != nil; current = current.parent {
		if current.records == r && current.index == index {
			return run(ctx)
		}
	}
	lock := r.demandLocks[index]
	select {
	case lock <- struct{}{}:
		defer func() { <-lock }()
		return run(context.WithValue(ctx, demandLockContextKey{}, &heldDemandLock{records: r, index: index, parent: held}))
	case <-ctx.Done():
		return ctx.Err()
	}
}
