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
		return stateVersion{}, err
	}
	loaded := stateVersion{Value: value, Version: object.Version}
	if value.ExpectedSize != request.ExpectedSize || value.OwnerFence != request.OwnerFence {
		loaded.Value.ExpectedSize = request.ExpectedSize
		loaded.Value.OwnerFence = request.OwnerFence
		if err := m.saveState(ctx, &loaded, request); err != nil {
			return stateVersion{}, err
		}
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
	value := state{SchemaVersion: stateSchemaVersion, Identity: request.Identity, Digest: request.Digest, Session: id, OwnerFence: request.OwnerFence, ExpectedSize: request.ExpectedSize, TotalSize: -1, CreatedAt: now, ProgressAt: now}
	encoded, _ := json.Marshal(value)
	entry, err := m.store.Put(ctx, key, bytes.NewReader(encoded), int64(len(encoded)), storage.PutOptions{IfAbsent: true})
	if errors.Is(err, storage.ErrAlreadyExists) {
		return m.loadOrCreate(ctx, request)
	}
	if err != nil {
		return stateVersion{}, err
	}
	loaded := stateVersion{Value: value, Version: entry.Version}
	reservation, err := initialReservation(request.ExpectedSize, m.chunkSize, m.maxReserved)
	if err == nil {
		loaded.Value.Reservation = reservation
		err = m.reserve(ctx, request, id, reservation)
	}
	if err == nil {
		err = m.saveState(ctx, &loaded, request)
	}
	if err != nil {
		_ = m.store.DeleteIfMatch(context.WithoutCancel(ctx), key, entry.Version)
		_ = m.release(context.WithoutCancel(ctx), request.Identity, request.Digest, id)
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
		loaded.Value.Reservation = required
		return m.saveState(ctx, loaded, request)
	}
	return nil
}

func (m *Manager) readState(ctx context.Context, key string) ([]byte, storage.Object, error) {
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return nil, storage.Object{}, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 1<<20))
	return data, object, err
}

func decodeState(data []byte, request Request) (state, error) {
	var value state
	if err := json.Unmarshal(data, &value); err != nil {
		return state{}, errors.Join(storage.ErrCorruptObject, err)
	}
	if value.SchemaVersion != stateSchemaVersion || value.Identity != request.Identity || value.Digest != request.Digest || value.Session == "" || value.ExpectedSize < -1 || value.Offset < 0 || value.Offset > request.MaxBytes || value.TotalSize < -1 || value.TotalSize >= 0 && value.Offset > value.TotalSize {
		return state{}, storage.ErrCorruptObject
	}
	return value, nil
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
