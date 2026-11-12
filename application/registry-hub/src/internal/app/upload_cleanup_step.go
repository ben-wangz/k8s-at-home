package app

import (
	"context"
	"errors"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (u *uploadSessions) cleanupEntry(ctx context.Context, gate *writeGate, entry storage.Entry) error {
	id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "uploads/sessions/"), ".json")
	state, err := u.readState(ctx, id)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil || !u.expired(state) {
		return err
	}
	leaseCtx, _, release, err := acquireRenewedLease(ctx, u.leases, "uploads/"+id, requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	state, err = u.readState(leaseCtx, id)
	if err != nil {
		return err
	}
	if !u.expired(state) {
		return nil
	}
	writeRelease, err := gate.Enter(leaseCtx)
	if errors.Is(err, ErrWritesClosed) {
		return nil
	}
	if err != nil {
		return err
	}
	defer writeRelease()
	return u.abortState(leaseCtx, state)
}

func (u *uploadSessions) expired(state uploadSession) bool {
	return time.Since(state.UpdatedAt) > u.idleTimeout || time.Since(state.CreatedAt) > u.maxLifetime
}
