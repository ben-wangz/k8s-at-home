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
	leaseCtx, _, release, err := acquireRenewedLease(ctx, u.leases, "uploads/"+id, requestID(), time.Minute)
	if errors.Is(err, lease.ErrHeld) {
		return nil
	}
	if err != nil {
		return err
	}
	defer release()
	state, err := u.readState(leaseCtx, id)
	if err != nil {
		return err
	}
	if time.Since(state.UpdatedAt) <= u.idleTimeout && time.Since(state.CreatedAt) <= u.maxLifetime {
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
