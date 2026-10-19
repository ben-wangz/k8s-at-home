package download

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
)

func (m *Manager) recoverChunks(ctx context.Context, loaded *stateVersion, request Request) error {
	if loaded.Value.Offset == 0 && loaded.Value.Index == "" {
		return nil
	}
	items, _, err := m.chunks(ctx, loaded.Value)
	if err != nil {
		if !errors.Is(err, storage.ErrCorruptObject) && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		reset, resetErr := m.reset(ctx, *loaded, request, false)
		if resetErr != nil {
			return resetErr
		}
		*loaded = reset
		return nil
	}
	valid := make([]chunk, 0, len(items))
	var offset int64
	for _, item := range items {
		if _, err := readVerifiedChunk(ctx, m.store, item); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(err, storage.ErrCorruptObject) && !errors.Is(err, storage.ErrNotFound) {
				return err
			}
			break
		}
		valid = append(valid, item)
		offset += item.Size
	}
	if len(valid) == len(items) {
		return nil
	}
	root, err := m.writeIndex(ctx, loaded.Value, valid)
	if err != nil {
		return err
	}
	loaded.Value.Index = root
	loaded.Value.Offset = offset
	if err := m.saveState(ctx, loaded, request); err != nil {
		return err
	}
	if request.Observe != nil {
		request.Observe("download_recovered_corrupt_chunk")
	}
	return nil
}

func (m *Manager) writeIndex(ctx context.Context, value state, items []chunk) (string, error) {
	var previous string
	for offset := 0; offset < len(items); offset += indexPageLimit {
		end := offset + indexPageLimit
		if end > len(items) {
			end = len(items)
		}
		page := indexPage{Previous: previous, Chunks: items[offset:end]}
		data, err := json.Marshal(page)
		if err != nil {
			return "", err
		}
		id, err := randomID()
		if err != nil {
			return "", err
		}
		key := indexPrefix(value.Identity, value.Digest, value.Session) + id + ".json"
		if _, err := m.putImmutable(ctx, key, data); err != nil {
			return "", err
		}
		previous = key
	}
	return previous, nil
}
