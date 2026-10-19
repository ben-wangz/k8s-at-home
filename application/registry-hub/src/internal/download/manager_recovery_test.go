package download

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
)

func TestOldFenceCannotCommitAfterOwnershipTakeover(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	oldManager := testManager(store)
	newManager := testManager(store)
	oldRequest := testRequest(content, digest, 20, nil)
	newRequest := testRequest(content, digest, 21, nil)
	oldState, err := oldManager.loadOrCreate(ctx, oldRequest)
	if err != nil {
		t.Fatalf("old owner loadOrCreate() error = %v", err)
	}
	newState, err := newManager.loadOrCreate(ctx, newRequest)
	if err != nil {
		t.Fatalf("new owner loadOrCreate() error = %v", err)
	}
	if newState.Value.OwnerFence != newRequest.OwnerFence {
		t.Fatalf("new state fence=%d, want %d", newState.Value.OwnerFence, newRequest.OwnerFence)
	}
	oldState.Value.Offset = 4
	oldState.Value.TotalSize = 8
	if err := oldManager.saveState(ctx, &oldState, oldRequest); !errors.Is(err, storage.ErrLeaseLost) {
		t.Fatalf("stale saveState() error = %v, want lease lost", err)
	}
	data, _, err := newManager.readState(ctx, stateKey(newRequest.Identity, digest))
	if err != nil {
		t.Fatalf("readState() error = %v", err)
	}
	current, err := decodeState(data, newRequest)
	if err != nil || current.OwnerFence != newRequest.OwnerFence || current.Offset != 0 {
		t.Fatalf("stored owner fence=%d offset=%d error=%v", current.OwnerFence, current.Offset, err)
	}
}

func TestCleanupReclaimsExpiredChunksAndQuota(t *testing.T) {
	ctx, store := testDownloadStore(t)
	content := []byte("abcdefgh")
	digest := artifact.CalculateSHA256(content)
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	manager := testManager(store)
	manager.now = func() time.Time { return created }
	calls := 0
	request := testRequest(content, digest, 22, func(_ context.Context, start, _ int64, _ string) (*http.Response, context.CancelFunc, error) {
		calls++
		if start > 0 {
			return nil, nil, errors.New("stop after saving the first prefix")
		}
		response := testResponse(http.StatusOK, content, int64(len(content)), &readThenError{reader: bytes.NewReader(content[:4])})
		response.Header.Set("Docker-Content-Digest", digest)
		return response, func() {}, nil
	})
	if _, err := manager.Fetch(ctx, request); err == nil || calls != 2 {
		t.Fatalf("Fetch() error=%v calls=%d, want interrupted transfer", err, calls)
	}
	removed, err := manager.Cleanup(ctx, created.Add(2*time.Hour), time.Hour, 10, func(ctx context.Context, identity, actualDigest string) (context.Context, func(), bool, error) {
		if identity != request.Identity || actualDigest != digest {
			t.Fatalf("cleanup lease identity=%q digest=%q", identity, actualDigest)
		}
		return ctx, func() {}, true, nil
	})
	if err != nil || removed != 1 {
		t.Fatalf("Cleanup() removed=%d error=%v, want 1 expired session", removed, err)
	}
	if _, _, err := manager.readState(ctx, stateKey(request.Identity, digest)); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("expired state remains, error=%v", err)
	}
	entries, _, err := store.List(ctx, partialObjectPrefix(request.Identity, digest)+"sessions/", "", 100)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expired session objects=%v error=%v", entries, err)
	}
	quota, _, err := manager.readQuota(ctx)
	if err != nil || len(quota.Reservations) != 0 {
		t.Fatalf("remaining quota reservations=%v error=%v", quota.Reservations, err)
	}
}
