package download

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"registry-hub/internal/storage"
)

func (m *Manager) deleteState(ctx context.Context, key, version string) error {
	err := m.store.DeleteIfMatch(ctx, key, version)
	if !errors.Is(err, storage.ErrUnknownCommit) {
		return err
	}
	current, headErr := m.store.Head(ctx, key)
	if errors.Is(headErr, storage.ErrNotFound) {
		return nil
	}
	if headErr != nil {
		return errors.Join(err, headErr)
	}
	if current.Version != version {
		return storage.ErrConditionFailed
	}
	return storage.ErrUnknownCommit
}

func (m *Manager) removeSessions(ctx context.Context, value state) error {
	sessions := append([]string(nil), value.GarbageSessions...)
	sessions = append(sessions, value.Session)
	seen := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		if session == "" {
			continue
		}
		if _, exists := seen[session]; exists {
			continue
		}
		seen[session] = struct{}{}
		current := value
		current.Session = session
		if err := m.removeSession(ctx, current); err != nil {
			return err
		}
	}
	return nil
}

func chunksPrefix(identity, digest, session string) string {
	return partialObjectPrefix(identity, digest) + "sessions/" + session + "/chunks/"
}

func indexPrefix(identity, digest, session string) string {
	return partialObjectPrefix(identity, digest) + "sessions/" + session + "/indexes/"
}

func partialObjectPrefix(identity, digest string) string {
	value := sha256.Sum256([]byte(identity + "\x00" + digest))
	return partialPrefix + hex.EncodeToString(value[:]) + "/"
}
