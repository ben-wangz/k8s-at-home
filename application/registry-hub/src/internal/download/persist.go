package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/storage"
)

func (m *Manager) saveState(ctx context.Context, loaded *stateVersion, request Request) error {
	return m.updateState(ctx, loaded, loaded.Value, request)
}

func (m *Manager) updateState(ctx context.Context, loaded *stateVersion, candidate state, request Request) error {
	updated, err := m.commitState(ctx, *loaded, candidate, request)
	if err != nil {
		return err
	}
	*loaded = updated
	return nil
}

func (m *Manager) commitState(ctx context.Context, committed stateVersion, candidate state, request Request) (stateVersion, error) {
	if candidate.OwnerFence != request.OwnerFence {
		return stateVersion{}, guardError(storage.ErrLeaseLost)
	}
	if committed.Value.Revision == ^uint64(0) {
		return stateVersion{}, persistenceError("state_commit", storage.ErrCorruptObject)
	}
	candidate = cloneState(candidate)
	candidate.Revision = committed.Value.Revision + 1
	if err := validateState(candidate, request); err != nil {
		return stateVersion{}, persistenceError("state_validate", err)
	}
	if request.Guard != nil {
		if err := request.Guard(ctx); err != nil {
			return stateVersion{}, guardError(err)
		}
	}
	data, err := json.Marshal(candidate)
	if err != nil {
		return stateVersion{}, persistenceError("state_encode", err)
	}
	key := stateKey(request.Identity, request.Digest)
	entry, err := m.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: committed.Version})
	if errors.Is(err, storage.ErrConditionFailed) {
		return stateVersion{}, guardError(storage.ErrLeaseLost)
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, object, readErr := m.readState(ctx, key)
		if readErr == nil && bytes.Equal(stored, data) {
			entry = storage.Entry{Key: key, Version: object.Version, Size: object.Size}
			err = nil
		} else {
			if readErr == nil {
				latest, validateErr := decodeState(stored, request)
				if validateErr == nil && latest.OwnerFence != candidate.OwnerFence {
					return stateVersion{}, guardError(storage.ErrLeaseLost)
				}
				readErr = validateErr
			}
			return stateVersion{}, persistenceError("state_commit", errors.Join(storage.ErrUnknownCommit, readErr))
		}
	}
	if err != nil {
		return stateVersion{}, persistenceError("state_commit", err)
	}
	if entry.Version == "" {
		return stateVersion{}, persistenceError("state_commit", errors.Join(storage.ErrUnknownCommit, storage.ErrCorruptObject))
	}
	return stateVersion{Value: candidate, Version: entry.Version}, nil
}

func cloneState(value state) state {
	value.GarbageSessions = append([]string(nil), value.GarbageSessions...)
	return value
}

func (m *Manager) appendChunk(ctx context.Context, loaded *stateVersion, request Request, start int64, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if int64(len(data)) > m.chunkSize || start != loaded.Value.Offset || start > request.MaxBytes-int64(len(data)) {
		return errors.New("download chunk exceeds its valid range")
	}
	if loaded.Value.ExpectedSize >= 0 && start+int64(len(data)) > loaded.Value.ExpectedSize || loaded.Value.TotalSize >= 0 && start+int64(len(data)) > loaded.Value.TotalSize {
		return errors.New("download chunk exceeds the expected object size")
	}
	reservation := reservationFor(start+int64(len(data)), m.maxReserved)
	if loaded.Value.ExpectedSize >= 0 {
		var err error
		reservation, err = initialReservation(loaded.Value.ExpectedSize, m.chunkSize, m.maxReserved)
		if err != nil {
			return err
		}
	}
	if err := m.reserve(ctx, request, loaded.Value.Session, reservation); err != nil {
		return err
	}
	if request.Guard != nil {
		if err := request.Guard(ctx); err != nil {
			return guardError(err)
		}
	}
	chunkID, err := randomID()
	if err != nil {
		return err
	}
	chunkKey := chunksPrefix(request.Identity, request.Digest, loaded.Value.Session) + chunkID
	hash := sha256.Sum256(data)
	entry, err := m.putImmutable(ctx, chunkKey, data, "chunk_write")
	if err != nil {
		return err
	}
	item := chunk{Key: chunkKey, Start: start, Size: int64(len(data)), Hash: hex.EncodeToString(hash[:]), Version: entry.Version}
	page, err := m.nextPage(ctx, loaded, item, request)
	if err != nil {
		return err
	}
	candidate := cloneState(loaded.Value)
	candidate.Index = page
	candidate.Offset += item.Size
	candidate.Reservation = reservation
	candidate.ProgressAt = m.now().UTC()
	updated, err := m.commitState(ctx, *loaded, candidate, request)
	if err != nil {
		return err
	}
	*loaded = updated
	if request.Observe != nil {
		request.Observe("download_persisted_chunk")
	}
	return nil
}

func (m *Manager) nextPage(ctx context.Context, loaded *stateVersion, item chunk, request Request) (string, error) {
	page := indexPage{Chunks: []chunk{item}}
	if loaded.Value.Index != "" {
		previous, err := m.readPage(ctx, loaded.Value.Index)
		if err != nil {
			return "", err
		}
		if len(previous.Chunks) < indexPageLimit {
			page = indexPage{Previous: previous.Previous, Chunks: append(append([]chunk(nil), previous.Chunks...), item)}
		} else {
			page.Previous = loaded.Value.Index
		}
	}
	data, err := json.Marshal(page)
	if err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	key := indexPrefix(loaded.Value.Identity, loaded.Value.Digest, loaded.Value.Session) + id + ".json"
	if request.Guard != nil {
		if err := request.Guard(ctx); err != nil {
			return "", guardError(err)
		}
	}
	if _, err := m.putImmutable(ctx, key, data, "index_write"); err != nil {
		return "", persistenceError("index_write", err)
	}
	return key, nil
}

func (m *Manager) readPage(ctx context.Context, key string) (indexPage, error) {
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return indexPage{}, persistenceError("index_read", err)
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 4<<20))
	if err != nil {
		return indexPage{}, persistenceError("index_read", err)
	}
	var page indexPage
	if err := json.Unmarshal(data, &page); err != nil || len(page.Chunks) == 0 || len(page.Chunks) > indexPageLimit {
		return indexPage{}, storage.ErrCorruptObject
	}
	return page, nil
}

func (m *Manager) chunks(ctx context.Context, value state) ([]chunk, []string, error) {
	var pages []indexPage
	var keys []string
	visited := make(map[string]struct{})
	key := value.Index
	for key != "" {
		if _, exists := visited[key]; exists || len(pages) >= 1_000_000 {
			return nil, nil, storage.ErrCorruptObject
		}
		visited[key] = struct{}{}
		page, err := m.readPage(ctx, key)
		if err != nil {
			return nil, nil, err
		}
		pages = append(pages, page)
		keys = append(keys, key)
		key = page.Previous
	}
	var result []chunk
	for index := len(pages) - 1; index >= 0; index-- {
		result = append(result, pages[index].Chunks...)
	}
	var offset int64
	for _, item := range result {
		if item.Key == "" || item.Size <= 0 || item.Start != offset || item.Version == "" || len(item.Hash) != 64 {
			return nil, nil, storage.ErrCorruptObject
		}
		if item.Size > value.Offset-offset {
			return nil, nil, storage.ErrCorruptObject
		}
		offset += item.Size
	}
	if offset != value.Offset {
		return nil, nil, storage.ErrCorruptObject
	}
	return result, keys, nil
}
