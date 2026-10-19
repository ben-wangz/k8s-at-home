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
	if loaded.Value.OwnerFence != request.OwnerFence {
		return storage.ErrLeaseLost
	}
	if request.Guard != nil {
		if err := request.Guard(ctx); err != nil {
			return err
		}
	}
	loaded.Value.Revision++
	data, err := json.Marshal(loaded.Value)
	if err != nil {
		return err
	}
	entry, err := m.store.Put(ctx, stateKey(request.Identity, request.Digest), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: loaded.Version})
	if errors.Is(err, storage.ErrConditionFailed) {
		return storage.ErrLeaseLost
	}
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, object, readErr := m.readState(ctx, stateKey(request.Identity, request.Digest))
		if readErr == nil && bytes.Equal(stored, data) {
			entry = storage.Entry{Key: stateKey(request.Identity, request.Digest), Version: object.Version, Size: object.Size}
			err = nil
		} else if readErr == nil || errors.Is(readErr, storage.ErrNotFound) {
			return storage.ErrLeaseLost
		} else {
			return errors.Join(err, readErr)
		}
	}
	if err != nil {
		loaded.Value.Revision--
		return err
	}
	loaded.Version = entry.Version
	return nil
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
			return err
		}
	}
	chunkID, err := randomID()
	if err != nil {
		return err
	}
	chunkKey := chunksPrefix(request.Identity, request.Digest, loaded.Value.Session) + chunkID
	hash := sha256.Sum256(data)
	entry, err := m.putImmutable(ctx, chunkKey, data)
	if err != nil {
		return err
	}
	item := chunk{Key: chunkKey, Start: start, Size: int64(len(data)), Hash: hex.EncodeToString(hash[:]), Version: entry.Version}
	page, err := m.nextPage(ctx, loaded, item)
	if err != nil {
		return err
	}
	loaded.Value.Index = page
	loaded.Value.Offset += item.Size
	previousReservation := loaded.Value.Reservation
	loaded.Value.Reservation = reservation
	loaded.Value.ProgressAt = m.now().UTC()
	if err := m.saveState(ctx, loaded, request); err != nil {
		loaded.Value.Offset -= item.Size
		loaded.Value.Reservation = previousReservation
		return err
	}
	if request.Observe != nil {
		request.Observe("download_persisted_chunk")
	}
	return nil
}

func (m *Manager) nextPage(ctx context.Context, loaded *stateVersion, item chunk) (string, error) {
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
	if _, err := m.putImmutable(ctx, key, data); err != nil {
		return "", err
	}
	return key, nil
}

func (m *Manager) readPage(ctx context.Context, key string) (indexPage, error) {
	object, err := m.store.Get(ctx, key)
	if err != nil {
		return indexPage{}, err
	}
	defer object.Body.Close()
	data, err := io.ReadAll(io.LimitReader(object.Body, 4<<20))
	if err != nil {
		return indexPage{}, err
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

func (m *Manager) putImmutable(ctx context.Context, key string, data []byte) (storage.Entry, error) {
	entry, err := m.store.Put(ctx, key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfAbsent: true})
	if !errors.Is(err, storage.ErrUnknownCommit) {
		return entry, err
	}
	object, readErr := m.store.Get(ctx, key)
	if readErr != nil {
		return storage.Entry{}, errors.Join(err, readErr)
	}
	defer object.Body.Close()
	stored, readErr := io.ReadAll(io.LimitReader(object.Body, int64(len(data))+1))
	if readErr == nil && object.Size == int64(len(data)) && bytes.Equal(stored, data) {
		return storage.Entry{Key: key, Version: object.Version, Size: object.Size, ModTime: object.ModTime}, nil
	}
	if readErr != nil {
		return storage.Entry{}, errors.Join(err, readErr)
	}
	return storage.Entry{}, storage.ErrUnknownCommit
}
