package download

import (
	"context"
	"errors"
	"slices"

	"registry-hub/internal/storage"
)

func (m *Manager) reset(ctx context.Context, loaded stateVersion, request Request, disableRange bool) (stateVersion, error) {
	id, err := randomID()
	if err != nil {
		return stateVersion{}, err
	}
	candidate := cloneState(loaded.Value)
	candidate.Session = id
	candidate.GarbageSessions = append(candidate.GarbageSessions, loaded.Value.Session)
	candidate.OwnerFence = request.OwnerFence
	candidate.TotalSize = -1
	candidate.Offset = 0
	candidate.Index = ""
	candidate.ETag = ""
	candidate.RangeDisabled = disableRange
	candidate.Fallbacks++
	candidate.ProgressAt = m.now().UTC()
	loaded, err = m.commitState(ctx, loaded, candidate, request)
	if err != nil {
		return stateVersion{}, persistenceError("state_reset", err)
	}
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
		if err := m.removeSessionGuarded(ctx, value, request.Guard); err != nil {
			var guardErr *GuardError
			if errors.As(err, &guardErr) {
				return err
			}
			remaining = append(remaining, session)
		}
	}
	if slices.Equal(loaded.Value.GarbageSessions, remaining) {
		return nil
	}
	candidate := cloneState(loaded.Value)
	candidate.GarbageSessions = remaining
	updated, err := m.commitState(ctx, *loaded, candidate, request)
	if err != nil {
		return persistenceError("state_cleanup", err)
	}
	*loaded = updated
	return nil
}

func (m *Manager) removeSession(ctx context.Context, value state) error {
	return m.removeSessionGuarded(ctx, value, nil)
}

func (m *Manager) removeSessionGuarded(ctx context.Context, value state, guard func(context.Context) error) error {
	prefix := partialObjectPrefix(value.Identity, value.Digest) + "sessions/" + value.Session + "/"
	token := ""
	for {
		if guard != nil {
			if err := guard(ctx); err != nil {
				return guardError(err)
			}
		}
		entries, next, err := m.store.List(ctx, prefix, token, 500)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if guard != nil {
				if err := guard(ctx); err != nil {
					return guardError(err)
				}
			}
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
