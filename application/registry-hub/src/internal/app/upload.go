package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/distribution"
	"registry-hub/internal/lease"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type uploadSession struct {
	SchemaVersion      int        `json:"schemaVersion"`
	ID                 string     `json:"id"`
	Token              string     `json:"token"`
	Repository         string     `json:"repository"`
	InternalRepository string     `json:"internalRepository"`
	InternalPath       string     `json:"internalPath"`
	InternalQuery      url.Values `json:"internalQuery"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
	version            string
}

type uploadSessions struct {
	store       storage.Store
	registry    *distribution.Client
	leases      *lease.Manager
	idleTimeout time.Duration
	maxLifetime time.Duration
}

func (u *uploadSessions) AbortAll(ctx context.Context) error {
	token := ""
	for {
		entries, next, err := u.store.List(ctx, "uploads/sessions/", token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := u.abort(ctx, entry); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		token = next
	}
}

func (u *uploadSessions) abort(ctx context.Context, entry storage.Entry) error {
	id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "uploads/sessions/"), ".json")
	leaseCtx, _, release, err := acquireRenewedLease(ctx, u.leases, "uploads/"+id, requestID(), 60*time.Second)
	if err != nil {
		return err
	}
	defer release()
	var state uploadSession
	object, err := u.store.Get(leaseCtx, entry.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	data, readErr := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	_ = object.Body.Close()
	if readErr != nil || len(data) > 1<<20 {
		return errors.New("upload session state is invalid during GC drain")
	}
	if err := json.Unmarshal(data, &state); err != nil || state.SchemaVersion != 1 || state.ID == "" || state.InternalPath == "" {
		return errors.New("upload session state is invalid during GC drain")
	}
	state.version = object.Version
	return u.abortState(leaseCtx, state)
}

func (u *uploadSessions) abortState(ctx context.Context, state uploadSession) error {
	response, err := u.registry.Do(ctx, http.MethodDelete, state.InternalPath, state.InternalQuery, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusNotFound {
		return errors.New("Distribution did not confirm upload cancellation")
	}
	return u.store.DeleteIfMatch(ctx, uploadKey(state.ID), state.version)
}

func (u *uploadSessions) CleanupExpired(ctx context.Context, gate *writeGate, limit int) (int, error) {
	if limit <= 0 || limit > 10000 {
		limit = 100
	}
	removed, examined := 0, 0
	token := ""
	for examined < limit {
		entries, next, err := u.store.List(ctx, "uploads/sessions/", token, min(500, limit-examined))
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			examined++
			id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "uploads/sessions/"), ".json")
			leaseCtx, _, release, err := acquireRenewedLease(ctx, u.leases, "uploads/"+id, requestID(), 60*time.Second)
			if errors.Is(err, lease.ErrHeld) {
				continue
			}
			if err != nil {
				return removed, err
			}
			state, err := u.readState(leaseCtx, id)
			if err != nil {
				_ = release()
				return removed, err
			}
			if time.Since(state.UpdatedAt) <= u.idleTimeout && time.Since(state.CreatedAt) <= u.maxLifetime {
				_ = release()
				continue
			}
			writeRelease, err := gate.Enter(leaseCtx)
			if errors.Is(err, ErrWritesClosed) {
				_ = release()
				return removed, nil
			}
			if err != nil {
				_ = release()
				return removed, err
			}
			err = u.abortState(leaseCtx, state)
			writeRelease()
			releaseErr := release()
			if err != nil {
				return removed, errors.Join(err, releaseErr)
			}
			if releaseErr != nil {
				return removed, releaseErr
			}
			removed++
			if removed >= limit {
				return removed, nil
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return removed, nil
}

func newUploadSessions(store storage.Store, registry *distribution.Client, leases *lease.Manager, idleTimeout, maxLifetime time.Duration) *uploadSessions {
	return &uploadSessions{store: store, registry: registry, leases: leases, idleTimeout: idleTimeout, maxLifetime: maxLifetime}
}

func (u *uploadSessions) Create(ctx context.Context, repository, internalRepository, location string) (string, error) {
	internalPath, internalQuery, err := u.registry.ParseLocation(location, internalRepository)
	if err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	state := uploadSession{
		SchemaVersion: 1, ID: id, Token: hex.EncodeToString(random[:]),
		Repository: repository, InternalRepository: internalRepository, InternalPath: internalPath, InternalQuery: internalQuery,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := u.save(ctx, &state); err != nil {
		return "", err
	}
	return u.externalLocation(repository, id, state.Token), nil
}
