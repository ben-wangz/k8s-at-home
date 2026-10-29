package download

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	"registry-hub/internal/storage"
)

func validateRequest(request Request) error {
	if request.Identity == "" || request.Digest == "" || request.MaxBytes <= 0 || request.OwnerFence == 0 || request.Source == nil {
		return errors.New("download request is incomplete")
	}
	if request.ExpectedSize < -1 || request.ExpectedSize > request.MaxBytes {
		return errors.New("download expected size is invalid")
	}
	if request.FinalizeTimeout < 0 {
		return errors.New("download finalization timeout is invalid")
	}
	return nil
}

func (m *Manager) loadOrCreate(ctx context.Context, request Request) (stateVersion, error) {
	key := stateKey(request.Identity, request.Digest)
	data, object, err := m.readState(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return m.createState(ctx, key, request)
	}
	if err != nil {
		return stateVersion{}, err
	}
	value, err := decodeState(data, request)
	if err != nil {
		return stateVersion{}, persistenceError("state_validate", err)
	}
	loaded := stateVersion{Value: value, Version: object.Version}
	if value.ExpectedSize != request.ExpectedSize || value.OwnerFence != request.OwnerFence || value.Authority == "" && request.Authority != "" {
		candidate := cloneState(loaded.Value)
		candidate.ExpectedSize = request.ExpectedSize
		candidate.OwnerFence = request.OwnerFence
		if request.Authority != "" {
			candidate.Authority, candidate.Repository = request.Authority, request.Repository
		}
		updated, err := m.commitState(ctx, loaded, candidate, request)
		if err != nil {
			return stateVersion{}, err
		}
		loaded = updated
	}
	if err := m.ensureReservation(ctx, &loaded, request); err != nil {
		return stateVersion{}, err
	}
	if err := m.cleanupGarbageSessions(ctx, &loaded, request); err != nil {
		return stateVersion{}, err
	}
	if err := m.recoverChunks(ctx, &loaded, request); err != nil {
		return stateVersion{}, err
	}
	return loaded, nil
}

func (m *Manager) createState(ctx context.Context, key string, request Request) (stateVersion, error) {
	id, err := randomID()
	if err != nil {
		return stateVersion{}, err
	}
	now := m.now().UTC()
	value := state{SchemaVersion: stateSchemaVersion, Authority: request.Authority, Repository: request.Repository, Identity: request.Identity, Digest: request.Digest, Session: id, OwnerFence: request.OwnerFence, ExpectedSize: request.ExpectedSize, TotalSize: -1, CreatedAt: now, ProgressAt: now}
	if request.Guard != nil {
		if err := request.Guard(ctx); err != nil {
			return stateVersion{}, guardError(err)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return stateVersion{}, persistenceError("state_encode", err)
	}
	entry, err := m.store.Put(ctx, key, bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) {
		return m.loadOrCreate(ctx, request)
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, object, readErr := m.readState(ctx, key)
		if readErr == nil && bytes.Equal(stored, encoded) {
			entry = storage.Entry{Key: key, Version: object.Version, Size: object.Size, ModTime: object.ModTime}
			err = nil
		} else {
			return stateVersion{}, persistenceError("state_create", errors.Join(storage.ErrUnknownCommit, readErr))
		}
	}
	if err != nil {
		return stateVersion{}, persistenceError("state_create", err)
	}
	if entry.Version == "" {
		return stateVersion{}, persistenceError("state_create", errors.Join(storage.ErrUnknownCommit, storage.ErrCorruptObject))
	}
	loaded := stateVersion{Value: value, Version: entry.Version}
	reservation, err := initialReservation(request.ExpectedSize, m.chunkSize, m.maxReserved)
	if err == nil {
		err = m.reserve(ctx, request, id, reservation)
	}
	if err == nil {
		candidate := cloneState(loaded.Value)
		candidate.Reservation = reservation
		loaded, err = m.commitState(ctx, loaded, candidate, request)
	}
	if err != nil {
		return stateVersion{}, err
	}
	return loaded, nil
}

func (m *Manager) ensureReservation(ctx context.Context, loaded *stateVersion, request Request) error {
	required := loaded.Value.Reservation
	if required == 0 {
		var err error
		required, err = initialReservation(loaded.Value.ExpectedSize, m.chunkSize, m.maxReserved)
		if err != nil {
			return err
		}
	}
	if err := m.reserve(ctx, request, loaded.Value.Session, required); err != nil {
		return err
	}
	if loaded.Value.Reservation == 0 {
		candidate := cloneState(loaded.Value)
		candidate.Reservation = required
		updated, err := m.commitState(ctx, *loaded, candidate, request)
		if err != nil {
			return err
		}
		*loaded = updated
	}
	return nil
}

func (m *Manager) readState(ctx context.Context, key string) ([]byte, storage.Object, error) {
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return nil, storage.Object{}, persistenceError("state_read", err)
	}
	defer object.Body.Close()
	if object.Version == "" {
		return nil, storage.Object{}, persistenceError("state_read", storage.ErrCorruptObject)
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	return data, object, persistenceError("state_read", err)
}

func decodeState(data []byte, request Request) (state, error) {
	var value state
	if err := json.Unmarshal(data, &value); err != nil {
		return state{}, errors.Join(storage.ErrCorruptObject, err)
	}
	if err := validateState(value, request); err != nil {
		return state{}, err
	}
	return value, nil
}

func validateState(value state, request Request) error {
	if value.SchemaVersion != stateSchemaVersion || value.Identity != request.Identity || value.Digest != request.Digest ||
		value.Session == "" || value.OwnerFence == 0 || value.ExpectedSize < -1 || value.ExpectedSize > request.MaxBytes ||
		value.Offset < 0 || value.Offset > request.MaxBytes || value.TotalSize < -1 || value.TotalSize > request.MaxBytes ||
		value.TotalSize >= 0 && value.Offset > value.TotalSize || value.Reservation < 0 || value.Fallbacks < 0 {
		return storage.ErrCorruptObject
	}
	for _, session := range value.GarbageSessions {
		if session == "" || session == value.Session {
			return storage.ErrCorruptObject
		}
	}
	return nil
}

func stateKey(identity, digest string) string {
	hash := sha256.Sum256([]byte(identity + "\x00" + digest))
	return partialPrefix + hex.EncodeToString(hash[:]) + "/state.json"
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func formatOffset(offset int64) string { return strconv.FormatInt(offset, 10) }
