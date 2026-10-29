package download

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"time"

	"registry-hub/internal/storage"
)

const maxActiveSessions = 128

var ErrQuotaExceeded = errors.New("partial download quota is exhausted")

type quotaReservation struct {
	Identity string `json:"identity,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Session  string `json:"session"`
	Bytes    int64  `json:"bytes"`
}

type quotaState struct {
	Reservations map[string]quotaReservation `json:"reservations"`
}

func (m *Manager) Usage(ctx context.Context) (int64, int, error) {
	current, _, err := m.readQuota(ctx)
	if errors.Is(err, storage.ErrNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var reserved int64
	for _, item := range current.Reservations {
		if item.Bytes < 0 || item.Bytes > math.MaxInt64-reserved {
			return 0, 0, storage.ErrCorruptObject
		}
		reserved += item.Bytes
	}
	return reserved, len(current.Reservations), nil
}

func (m *Manager) reserve(ctx context.Context, request Request, session string, required int64) error {
	if required < 0 || required > m.maxReserved {
		return ErrQuotaExceeded
	}
	key := stateKey(request.Identity, request.Digest)
	for attempt := 0; attempt < 16; attempt++ {
		current, version, err := m.readQuota(ctx)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		if current.Reservations == nil {
			current.Reservations = make(map[string]quotaReservation)
		}
		old, exists := current.Reservations[key]
		if exists && (old.Session == "" || old.Bytes < 0) {
			return persistenceError("quota_validate", storage.ErrCorruptObject)
		}
		if exists && required < old.Bytes {
			required = old.Bytes
		}
		used := int64(0)
		for reservationKey, reservation := range current.Reservations {
			if reservationKey != key {
				if reservation.Session == "" || reservation.Bytes < 0 || reservation.Bytes > math.MaxInt64-used {
					return persistenceError("quota_validate", storage.ErrCorruptObject)
				}
				used += reservation.Bytes
			}
		}
		if !exists && len(current.Reservations) >= maxActiveSessions || used > m.maxReserved || required > m.maxReserved-used {
			return ErrQuotaExceeded
		}
		if request.Guard != nil {
			if err := request.Guard(ctx); err != nil {
				return guardError(err)
			}
		}
		if exists && old.Session == session && old.Bytes >= required && old.Identity == request.Identity && old.Digest == request.Digest {
			return nil
		}
		current.Reservations[key] = quotaReservation{Session: session, Bytes: required, Identity: request.Identity, Digest: request.Digest}
		data, err := json.Marshal(current)
		if err != nil {
			return err
		}
		options := storage.PutOptions{IfAbsent: version == ""}
		if version != "" {
			options.IfMatch = version
			options.IfAbsent = false
		}
		if _, err := m.store.Put(ctx, quotaKey, bytes.NewReader(data), int64(len(data)), options); errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrAlreadyExists) {
			if attempt == 15 {
				return persistenceError("quota_reserve", storage.ErrConditionFailed)
			}
			if err := m.waitQuota(ctx, attempt); err != nil {
				return guardError(err)
			}
			continue
		} else if errors.Is(err, storage.ErrUnknownCommit) {
			latest, _, readErr := m.readQuota(ctx)
			if readErr == nil {
				committed := latest.Reservations[key]
				if committed.Session == session && committed.Bytes >= required {
					return nil
				}
			}
			return persistenceError("quota_reserve_confirm", errors.Join(err, readErr))
		} else if err != nil {
			return persistenceError("quota_reserve", err)
		}
		return nil
	}
	return persistenceError("quota_reserve", storage.ErrConditionFailed)
}

func (m *Manager) waitQuota(ctx context.Context, conflict int) error {
	if m.quotaWait == nil {
		return waitQuotaConflict(ctx, conflict)
	}
	return m.quotaWait(ctx, conflict)
}

func waitQuotaConflict(ctx context.Context, conflict int) error {
	limit := quotaBackoffLimit(conflict)
	delay := time.Duration(rand.Int64N(int64(limit) + 1))
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func quotaBackoffLimit(conflict int) time.Duration {
	const maximum = 250 * time.Millisecond
	if conflict < 0 {
		conflict = 0
	}
	if conflict >= 5 {
		return maximum
	}
	limit := 10 * time.Millisecond * time.Duration(1<<conflict)
	if limit > maximum {
		return maximum
	}
	return limit
}

func (m *Manager) readQuota(ctx context.Context) (quotaState, string, error) {
	object, err := m.store.Get(ctx, quotaKey)
	if err != nil {
		return quotaState{}, "", persistenceError("quota_read", err)
	}
	defer object.Body.Close()
	var result quotaState
	if err := json.NewDecoder(io.LimitReader(object.Body, 4<<20)).Decode(&result); err != nil || result.Reservations == nil {
		return quotaState{}, "", persistenceError("quota_decode", storage.ErrCorruptObject)
	}
	for _, reservation := range result.Reservations {
		if reservation.Session == "" || reservation.Bytes < 0 {
			return quotaState{}, "", persistenceError("quota_validate", storage.ErrCorruptObject)
		}
	}
	return result, object.Version, nil
}

func initialReservation(size, chunkSize, maxBytes int64) (int64, error) {
	base := size
	if base < 0 {
		base = chunkSize
	}
	if base > math.MaxInt64/2 {
		return 0, ErrQuotaExceeded
	}
	result := 2 * base
	if result > maxBytes {
		return 0, ErrQuotaExceeded
	}
	return result, nil
}

func reservationFor(offset int64, maxBytes int64) int64 {
	if offset > math.MaxInt64/2 {
		return maxBytes
	}
	reserved := offset * 2
	if reserved > maxBytes {
		return maxBytes
	}
	return reserved
}

const quotaKey = "downloads/v1/quota.json"
