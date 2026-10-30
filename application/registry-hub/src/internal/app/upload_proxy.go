package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/route"
	"registry-hub/internal/storage"
	"time"
)

func (u *uploadSessions) Proxy(request *http.Request, repository, uploadID string, query url.Values, gate *writeGate, beforeWrite func() error) (*http.Response, error) {
	leaseCtx, _, releaseLease, err := acquireRenewedLease(request.Context(), u.leases, "uploads/"+uploadID, requestID(), 60*time.Second)
	if err != nil {
		return nil, err
	}
	var releaseGate func()
	leaseReleased := false
	defer func() {
		if releaseGate != nil {
			_ = releaseLease()
			leaseReleased = true
			releaseGate()
		}
		if !leaseReleased {
			_ = releaseLease()
		}
	}()
	state, err := u.get(leaseCtx, uploadID)
	if err != nil {
		return nil, err
	}
	if state.Repository != repository || len(query["_state"]) != 1 || query.Get("_state") != state.Token {
		return nil, errors.New("upload session token is invalid")
	}
	for key, values := range query {
		if key != "_state" && key != "digest" && key != "digest-algorithm" {
			return nil, errors.New("upload session query contains an unsupported parameter")
		}
		if key == "digest" && len(values) == 0 {
			return nil, errors.New("upload digest is empty")
		}
		if key == "digest" {
			for _, value := range values {
				if !route.ValidDigest(value) {
					return nil, errors.New("upload digest is invalid")
				}
			}
		}
	}
	internalQuery := cloneValues(state.InternalQuery)
	for _, digest := range query["digest"] {
		internalQuery.Add("digest", digest)
	}
	for _, algorithm := range query["digest-algorithm"] {
		internalQuery.Add("digest-algorithm", algorithm)
	}
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		releaseGate, err = gate.Enter(leaseCtx)
		if err != nil {
			return nil, err
		}
		if beforeWrite != nil {
			if err := beforeWrite(); err != nil {
				return nil, err
			}
		}
	}
	response, err := u.proxyUploadAttempt(leaseCtx, request, state, internalQuery)
	if err != nil {
		return nil, err
	}
	state.RequestCount++
	state.LastHTTPStatus = response.StatusCode
	if (request.Method == http.MethodPatch || request.Method == http.MethodGet || request.Method == http.MethodHead) && (response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent) {
		if bytes, ok := acceptedUploadBytes(response.Header.Get("Range")); ok {
			state.ReceivedBytes = &bytes
		}
	}
	if request.Method == http.MethodPatch && response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		if err := u.recoverRange(leaseCtx, &state, response); err != nil {
			_ = response.Body.Close()
			return nil, errors.Join(storage.ErrUnavailable, err)
		}
	}
	if request.Method != http.MethodDelete && (response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent) {
		location := response.Header.Get("Location")
		if location != "" {
			newPath, newQuery, parseErr := u.registry.ParseLocation(location, state.InternalRepository)
			if parseErr != nil {
				_ = response.Body.Close()
				return nil, parseErr
			}
			state.InternalPath = newPath
			state.InternalQuery = newQuery
			state.Token, err = randomToken()
			if err != nil {
				_ = response.Body.Close()
				return nil, err
			}
			state.UpdatedAt = time.Now().UTC()
			if err := u.save(leaseCtx, &state); err != nil {
				_ = response.Body.Close()
				return nil, err
			}
			response.Header.Set("Location", u.externalLocation(repository, state.ID, state.Token))
		}
	}
	if response.StatusCode == http.StatusCreated || request.Method == http.MethodDelete && response.StatusCode == http.StatusNoContent {
		u.recordUploadCompletion(leaseCtx, state, uploadCompletionResult(request.Method, response.StatusCode), query.Get("digest"), response.Header.Get("Range"))
		if err := u.store.DeleteIfMatch(leaseCtx, uploadKey(state.ID), state.version); err != nil && !errors.Is(err, storage.ErrNotFound) {
			_ = response.Body.Close()
			return nil, err
		}
	}
	if response.Header.Get("Docker-Upload-UUID") != "" {
		response.Header.Set("Docker-Upload-UUID", state.ID)
	}
	return response, nil
}

func (u *uploadSessions) recoverRange(ctx context.Context, state *uploadSession, response *http.Response) error {
	response.Header.Set("Location", u.externalLocation(state.Repository, state.ID, state.Token))
	status, err := u.registry.Do(ctx, http.MethodGet, state.InternalPath, state.InternalQuery, make(http.Header), nil, 0)
	if err != nil {
		return err
	}
	defer status.Body.Close()
	if status.StatusCode != http.StatusNoContent {
		return errors.New("Distribution upload status could not be confirmed after an invalid range")
	}
	if currentRange := status.Header.Get("Range"); currentRange != "" {
		response.Header.Set("Range", currentRange)
	}
	location := status.Header.Get("Location")
	if location == "" {
		return nil
	}
	path, query, err := u.registry.ParseLocation(location, state.InternalRepository)
	if err != nil {
		return err
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	state.InternalPath = path
	state.InternalQuery = query
	state.Token = token
	state.UpdatedAt = time.Now().UTC()
	if err := u.save(ctx, state); err != nil {
		return err
	}
	response.Header.Set("Location", u.externalLocation(state.Repository, state.ID, state.Token))
	return nil
}

func (u *uploadSessions) RewriteCreated(ctx context.Context, response *http.Response, repository, internalRepository string) error {
	location := response.Header.Get("Location")
	if location == "" || response.StatusCode != http.StatusAccepted {
		return nil
	}
	external, err := u.Create(ctx, repository, internalRepository, location)
	if err != nil {
		return err
	}
	response.Header.Set("Location", external)
	return nil
}

func (u *uploadSessions) get(ctx context.Context, id string) (uploadSession, error) {
	state, err := u.readState(ctx, id)
	if err != nil {
		return uploadSession{}, err
	}
	if time.Since(state.UpdatedAt) > u.idleTimeout || time.Since(state.CreatedAt) > u.maxLifetime {
		return uploadSession{}, storage.ErrNotFound
	}
	return state, nil
}

func (u *uploadSessions) readState(ctx context.Context, id string) (uploadSession, error) {
	if len(id) != 32 {
		return uploadSession{}, storage.ErrNotFound
	}
	if _, err := hex.DecodeString(id); err != nil {
		return uploadSession{}, storage.ErrNotFound
	}
	object, err := u.store.Get(ctx, uploadKey(id))
	if err != nil {
		return uploadSession{}, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return uploadSession{}, errors.New("upload session state is invalid")
	}
	var state uploadSession
	if err := json.Unmarshal(data, &state); err != nil || state.SchemaVersion != 1 || state.ID != id {
		return uploadSession{}, errors.New("upload session state is invalid")
	}
	state.version = object.Version
	return state, nil
}

func (u *uploadSessions) save(ctx context.Context, state *uploadSession) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	options := storage.PutOptions{IfAbsent: state.version == ""}
	if state.version != "" {
		options.IfMatch = state.version
	}
	entry, err := u.store.Put(ctx, uploadKey(state.ID), bytes.NewReader(data), int64(len(data)), options)
	if err != nil {
		return err
	}
	state.version = entry.Version
	return nil
}

func (u *uploadSessions) externalLocation(repository, id, token string) string {
	query := url.Values{"_state": []string{token}}
	return "/v2/" + repository + "/blobs/uploads/" + id + "?" + query.Encode()
}

func uploadKey(id string) string { return "uploads/sessions/" + id + ".json" }

func randomToken() (string, error) {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(random[:]), nil
}

func cloneValues(source url.Values) url.Values {
	copy := make(url.Values, len(source))
	for key, values := range source {
		copy[key] = append([]string(nil), values...)
	}
	return copy
}
