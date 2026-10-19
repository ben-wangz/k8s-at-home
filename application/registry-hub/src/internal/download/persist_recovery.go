package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"registry-hub/internal/storage"
)

func (m *Manager) reset(ctx context.Context, loaded stateVersion, request Request, disableRange bool) (stateVersion, error) {
	previous := loaded.Value
	id, err := randomID()
	if err != nil {
		return stateVersion{}, err
	}
	loaded.Value.Session = id
	loaded.Value.GarbageSessions = append(loaded.Value.GarbageSessions, previous.Session)
	loaded.Value.OwnerFence = request.OwnerFence
	loaded.Value.Revision++
	loaded.Value.TotalSize = -1
	loaded.Value.Offset = 0
	loaded.Value.Index = ""
	loaded.Value.ETag = ""
	loaded.Value.RangeDisabled = disableRange
	loaded.Value.Fallbacks++
	loaded.Value.ProgressAt = m.now().UTC()
	data, err := json.Marshal(loaded.Value)
	if err != nil {
		return stateVersion{}, err
	}
	entry, err := m.store.Put(ctx, stateKey(request.Identity, request.Digest), bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: loaded.Version})
	if errors.Is(err, storage.ErrUnknownCommit) {
		stored, object, readErr := m.readState(ctx, stateKey(request.Identity, request.Digest))
		if readErr == nil && bytes.Equal(stored, data) {
			entry = storage.Entry{Key: stateKey(request.Identity, request.Digest), Version: object.Version, Size: object.Size}
			err = nil
		} else if readErr != nil {
			return stateVersion{}, errors.Join(err, readErr)
		} else {
			return stateVersion{}, storage.ErrLeaseLost
		}
	}
	if err != nil {
		return stateVersion{}, fmt.Errorf("replace invalid partial download state: %w", err)
	}
	loaded.Version = entry.Version
	if request.Observe != nil {
		request.Observe("download_fallback")
	}
	if loaded.Value.Reservation > 0 {
		if err := m.reserve(ctx, request, id, loaded.Value.Reservation); err != nil {
			return stateVersion{}, err
		}
	}
	if err := m.cleanupGarbageSessions(ctx, &loaded, request); err != nil {
		return stateVersion{}, err
	}
	return loaded, nil
}

func (m *Manager) cleanupGarbageSessions(ctx context.Context, loaded *stateVersion, request Request) error {
	if len(loaded.Value.GarbageSessions) == 0 {
		return nil
	}
	remaining := make([]string, 0, len(loaded.Value.GarbageSessions))
	for _, session := range loaded.Value.GarbageSessions {
		value := loaded.Value
		value.Session = session
		if err := m.removeSession(ctx, value); err != nil {
			remaining = append(remaining, session)
		}
	}
	loaded.Value.GarbageSessions = remaining
	return m.saveState(ctx, loaded, request)
}

func (m *Manager) removeSession(ctx context.Context, value state) error {
	prefix := partialObjectPrefix(value.Identity, value.Digest) + "sessions/" + value.Session + "/"
	token := ""
	for {
		entries, next, err := m.store.List(ctx, prefix, token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := m.store.DeleteIfMatch(ctx, entry.Key, entry.Version); err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
				if errors.Is(err, storage.ErrUnknownCommit) {
					if _, headErr := m.store.Head(ctx, entry.Key); errors.Is(headErr, storage.ErrNotFound) {
						continue
					}
				}
				return err
			}
		}
		if next == "" {
			break
		}
		token = next
	}
	return nil
}
