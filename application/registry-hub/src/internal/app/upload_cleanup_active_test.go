package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"registry-hub/internal/lease"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type uploadCleanupProbe struct {
	storage.Store
	leaseWrites atomic.Int32
	aborts      atomic.Int32
	beforeLease func()
}

func (s *uploadCleanupProbe) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.HasPrefix(key, "leases/uploads/") {
		s.leaseWrites.Add(1)
		if s.beforeLease != nil {
			hook := s.beforeLease
			s.beforeLease = nil
			hook()
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func uploadCleanupFixture(t *testing.T) (*uploadSessions, *writeGate, *uploadCleanupProbe, uploadSession) {
	t.Helper()
	f := newStagedAcceptanceFixture(t, 1<<20)
	store := &uploadCleanupProbe{Store: f.service.store}
	registry := newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected Distribution request: %s", r.Method)
		}
		store.aborts.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	u := newUploadSessions(store, registry, lease.NewManager(store), time.Minute, time.Hour)
	state := uploadSession{SchemaVersion: 1, ID: strings.Repeat("a", 32), Token: "private-token",
		Repository: "test.invalid/team/image", InternalRepository: "etest_2einvalid/team/image",
		InternalPath: "/v2/etest_2einvalid/team/image/blobs/uploads/mock", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := u.save(context.Background(), &state); err != nil {
		t.Fatal(err)
	}
	return u, f.service.gate, store, state
}

func TestUploadCleanupSkipsFreshSessionsWithoutLeaseWrites(t *testing.T) {
	u, gate, store, state := uploadCleanupFixture(t)
	ctx := context.Background()
	if err := u.cleanupEntry(ctx, gate, storage.Entry{Key: uploadKey(state.ID)}); err != nil {
		t.Fatal(err)
	}
	removed, err := u.CleanupExpired(ctx, gate, 100)
	if err != nil || removed != 0 || store.leaseWrites.Load() != 0 || store.aborts.Load() != 0 {
		t.Fatalf("fresh cleanup removed=%d lease writes=%d aborts=%d error=%v", removed, store.leaseWrites.Load(), store.aborts.Load(), err)
	}
}

func TestUploadCleanupRechecksExpiryAfterLeaseAcquisition(t *testing.T) {
	for _, mode := range []string{"entry", "scan"} {
		t.Run(mode, func(t *testing.T) {
			u, gate, store, state := uploadCleanupFixture(t)
			ctx := context.Background()
			state.UpdatedAt = time.Now().Add(-2 * time.Minute)
			if err := u.save(ctx, &state); err != nil {
				t.Fatal(err)
			}
			store.beforeLease = func() {
				state.UpdatedAt = time.Now().UTC()
				if err := u.save(ctx, &state); err != nil {
					t.Fatal(err)
				}
			}
			if err := runUploadCleanup(ctx, u, gate, state, mode); err != nil {
				t.Fatal(err)
			}
			if _, err := u.readState(ctx, state.ID); err != nil || store.aborts.Load() != 0 || store.leaseWrites.Load() < 2 {
				t.Fatalf("refreshed session error=%v aborts=%d lease writes=%d", err, store.aborts.Load(), store.leaseWrites.Load())
			}
		})
	}
}

func TestUploadCleanupPreservesIdleAndLifetimeExpiry(t *testing.T) {
	for _, mode := range []string{"entry", "scan"} {
		for _, expiry := range []string{"idle", "lifetime"} {
			t.Run(mode+"/"+expiry, func(t *testing.T) {
				u, gate, store, state := uploadCleanupFixture(t)
				ctx := context.Background()
				if expiry == "idle" {
					state.UpdatedAt = time.Now().Add(-2 * time.Minute)
				} else {
					state.CreatedAt = time.Now().Add(-2 * time.Hour)
				}
				if err := u.save(ctx, &state); err != nil {
					t.Fatal(err)
				}
				if err := runUploadCleanup(ctx, u, gate, state, mode); err != nil {
					t.Fatal(err)
				}
				if _, err := u.readState(ctx, state.ID); !errors.Is(err, storage.ErrNotFound) || store.aborts.Load() != 1 || store.leaseWrites.Load() < 2 {
					t.Fatalf("expired session error=%v aborts=%d lease writes=%d", err, store.aborts.Load(), store.leaseWrites.Load())
				}
			})
		}
	}
}

func runUploadCleanup(ctx context.Context, u *uploadSessions, gate *writeGate, state uploadSession, mode string) error {
	if mode == "entry" {
		return u.cleanupEntry(ctx, gate, storage.Entry{Key: uploadKey(state.ID)})
	}
	_, err := u.CleanupExpired(ctx, gate, 100)
	return err
}

func TestUploadProxyContentionIsRetryableAndInvalidTokenRemainsRejected(t *testing.T) {
	u, gate, store, state := uploadCleanupFixture(t)
	ctx := context.Background()
	held, err := u.leases.Acquire(ctx, "uploads/"+state.ID, "maintenance", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{uploads: u, gate: gate}
	service.cfg.Limits.UploadBytes = 1 << 20
	routed := route.Route{Repository: state.Repository, Internal: state.InternalRepository, UploadID: state.ID}
	request := httptest.NewRequest(http.MethodPatch, "/v2/"+state.Repository+"/blobs/uploads/"+state.ID, strings.NewReader("chunk"))
	response := httptest.NewRecorder()
	service.proxyUpload(response, request, routed, url.Values{"_state": {state.Token}}, &requestMeta{})
	if response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "1" || store.aborts.Load() != 0 {
		t.Fatalf("busy session status=%d retry=%q requests=%d", response.Code, response.Header().Get("Retry-After"), store.aborts.Load())
	}
	if err := u.leases.Release(ctx, held); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	service.proxyUpload(response, request, routed, url.Values{"_state": {"invalid-token"}}, &requestMeta{})
	if response.Code != http.StatusBadRequest || store.aborts.Load() != 0 {
		t.Fatalf("invalid token status=%d requests=%d", response.Code, store.aborts.Load())
	}
	if _, err := u.readState(ctx, state.ID); err != nil {
		t.Fatalf("contention or invalid token removed the valid session: %v", err)
	}
}
