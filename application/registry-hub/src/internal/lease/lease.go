package lease

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"registry-hub/internal/storage"
)

var (
	ErrHeld = errors.New("lease is held by another owner")
	ErrLost = errors.New("lease fencing token was lost")
)

const (
	leaseSchemaPrevious = 1
	leaseSchemaCurrent  = 2
)

type record struct {
	SchemaVersion int       `json:"schemaVersion"`
	Owner         string    `json:"owner"`
	FencingToken  uint64    `json:"fencingToken"`
	AcquiredAt    time.Time `json:"acquiredAt"`
	ExpiresAt     time.Time `json:"expiresAt"`
	State         string    `json:"state"`
}

type Lease struct {
	Key          string
	Owner        string
	FencingToken uint64
	Version      string
	ExpiresAt    time.Time
}

type Manager struct {
	store storage.Store
}

func NewManager(store storage.Store) *Manager {
	return &Manager{store: store}
}

func (m *Manager) Acquire(ctx context.Context, key, owner string, ttl time.Duration) (Lease, error) {
	storageKey := "leases/" + key
	for attempt := 0; attempt < 3; attempt++ {
		previous, current, err := m.read(ctx, storageKey)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return Lease{}, err
		}
		if err == nil && current.State == "held" && current.ExpiresAt.After(time.Now()) {
			return Lease{}, ErrHeld
		}
		now := time.Now().UTC()
		next := record{SchemaVersion: leaseSchemaCurrent, Owner: owner, FencingToken: current.FencingToken + 1, AcquiredAt: now, ExpiresAt: now.Add(ttl), State: "held"}
		data, _ := json.Marshal(next)
		options := storage.PutOptions{IfAbsent: errors.Is(err, storage.ErrNotFound)}
		if err == nil {
			options.IfMatch = previous.Version
		}
		entry, putErr := m.store.Put(ctx, storageKey, bytes.NewReader(data), int64(len(data)), options)
		if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrAlreadyExists) {
			continue
		}
		if putErr != nil {
			return Lease{}, putErr
		}
		return Lease{Key: storageKey, Owner: owner, FencingToken: next.FencingToken, Version: entry.Version, ExpiresAt: next.ExpiresAt}, nil
	}
	return Lease{}, storage.ErrConditionFailed
}

func (m *Manager) Renew(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	previous, current, err := m.read(ctx, lease.Key)
	if err != nil {
		return Lease{}, err
	}
	if !matches(current, lease) || !current.ExpiresAt.After(time.Now()) {
		return Lease{}, ErrLost
	}
	now := time.Now().UTC()
	current.ExpiresAt = now.Add(ttl)
	data, _ := json.Marshal(current)
	entry, err := m.store.Put(ctx, lease.Key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: previous.Version})
	if err != nil {
		if errors.Is(err, storage.ErrConditionFailed) {
			return Lease{}, ErrLost
		}
		return Lease{}, err
	}
	lease.Version = entry.Version
	lease.ExpiresAt = current.ExpiresAt
	return lease, nil
}

func (m *Manager) Validate(ctx context.Context, lease Lease) error {
	_, current, err := m.read(ctx, lease.Key)
	if err != nil {
		return err
	}
	if !matches(current, lease) || !current.ExpiresAt.After(time.Now()) {
		return ErrLost
	}
	return nil
}

func (m *Manager) Active(ctx context.Context, key string) (bool, error) {
	_, current, err := m.read(ctx, "leases/"+key)
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return current.State == "held" && current.ExpiresAt.After(time.Now()), nil
}

func (m *Manager) Release(ctx context.Context, lease Lease) error {
	previous, current, err := m.read(ctx, lease.Key)
	if err != nil {
		return err
	}
	if !matches(current, lease) {
		return ErrLost
	}
	current.State = "released"
	current.ExpiresAt = time.Now().UTC()
	data, _ := json.Marshal(current)
	_, err = m.store.Put(ctx, lease.Key, bytes.NewReader(data), int64(len(data)), storage.PutOptions{IfMatch: previous.Version})
	if errors.Is(err, storage.ErrConditionFailed) {
		return ErrLost
	}
	return err
}

func (m *Manager) read(ctx context.Context, key string) (storage.Object, record, error) {
	for attempt := 0; attempt < 16; attempt++ {
		object, err := m.store.Get(ctx, key)
		if err != nil {
			return storage.Object{}, record{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(object.Body, (1<<20)+1))
		closeErr := object.Body.Close()
		if readErr != nil {
			return storage.Object{}, record{}, readErr
		}
		if closeErr != nil {
			return storage.Object{}, record{}, closeErr
		}
		if len(data) > 1<<20 {
			return storage.Object{}, record{}, errors.New("lease state exceeds size limit")
		}
		var value record
		if err := json.Unmarshal(data, &value); err != nil {
			return storage.Object{}, record{}, fmt.Errorf("decode lease state: %w", err)
		}
		if value.FencingToken == 0 {
			return storage.Object{}, record{}, errors.New("lease state has an unsupported schema or fencing token")
		}
		switch value.SchemaVersion {
		case leaseSchemaCurrent:
			return object, value, nil
		case leaseSchemaPrevious:
			value.SchemaVersion = leaseSchemaCurrent
			updated, marshalErr := json.Marshal(value)
			if marshalErr != nil {
				return storage.Object{}, record{}, marshalErr
			}
			entry, putErr := m.store.Put(ctx, key, bytes.NewReader(updated), int64(len(updated)), storage.PutOptions{IfMatch: object.Version})
			if errors.Is(putErr, storage.ErrConditionFailed) || errors.Is(putErr, storage.ErrUnknownCommit) {
				continue
			}
			if putErr != nil {
				return storage.Object{}, record{}, putErr
			}
			object.Version = entry.Version
			return object, value, nil
		default:
			return storage.Object{}, record{}, errors.New("lease state has an unsupported schema or fencing token")
		}
	}
	return storage.Object{}, record{}, storage.ErrUnavailable
}

func matches(current record, lease Lease) bool {
	return current.State == "held" && current.Owner == lease.Owner && current.FencingToken == lease.FencingToken
}
