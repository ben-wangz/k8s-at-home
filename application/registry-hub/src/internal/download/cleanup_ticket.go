package download

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/storage"
	"time"
)

type CleanupTicket struct {
	Identity     string    `json:"identity"`
	Digest       string    `json:"digest"`
	Session      string    `json:"session"`
	Sessions     []string  `json:"sessions"`
	OwnerFence   uint64    `json:"ownerFence"`
	StateVersion string    `json:"stateVersion"`
	Bytes        int64     `json:"bytes"`
	CompletedAt  time.Time `json:"completedAt"`
}

type CleanupProgress struct {
	SessionOffset int    `json:"sessionOffset"`
	Token         string `json:"token"`
	Phase         int    `json:"phase"`
}

func (m *Manager) CompletedTicket(ctx context.Context, identity, digest string, fence uint64) (CleanupTicket, error) {
	data, object, err := m.readState(ctx, stateKey(identity, digest))
	if err != nil {
		return CleanupTicket{}, err
	}
	value, err := decodeState(data, Request{Identity: identity, Digest: digest, MaxBytes: int64(^uint64(0) >> 1)})
	if err != nil {
		return CleanupTicket{}, err
	}
	if value.TotalSize < 0 || value.Offset != value.TotalSize || fence != 0 && value.OwnerFence != fence {
		return CleanupTicket{}, storage.ErrLeaseLost
	}
	return CleanupTicket{Identity: identity, Digest: digest, Session: value.Session, Sessions: append(append([]string(nil), value.GarbageSessions...), value.Session), OwnerFence: value.OwnerFence, StateVersion: object.Version, Bytes: value.Reservation, CompletedAt: value.ProgressAt}, nil
}

func (m *Manager) CleanupCommittedStep(ctx context.Context, ticket CleanupTicket, p *CleanupProgress, guard func(context.Context) error) (bool, error) {
	if ticket.Identity == "" || ticket.Session == "" || len(ticket.Sessions) == 0 {
		return false, storage.ErrCorruptObject
	}
	if err := guard(ctx); err != nil {
		return false, err
	}
	data, object, err := m.readState(ctx, stateKey(ticket.Identity, ticket.Digest))
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return false, err
	}
	if err == nil {
		var current state
		if json.Unmarshal(data, &current) != nil {
			return false, storage.ErrCorruptObject
		}
		// A resumed session needs a new ticket, even when it reuses the same chunk namespace.
		if current.Session == ticket.Session && object.Version != ticket.StateVersion {
			return true, nil
		}
	}
	if p.Phase == 0 && p.SessionOffset < len(ticket.Sessions) {
		prefix := partialObjectPrefix(ticket.Identity, ticket.Digest) + "sessions/" + ticket.Sessions[p.SessionOffset] + "/"
		entries, next, err := m.store.List(ctx, prefix, p.Token, 16)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if err := guard(ctx); err != nil {
				return false, err
			}
			err := m.store.DeleteIfMatch(ctx, entry.Key, entry.Version)
			if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrConditionFailed) {
				return false, err
			}
		}
		p.Token = next
		if next == "" {
			p.SessionOffset++
			p.Token = ""
		}
		return false, nil
	}
	if p.Phase == 0 {
		p.Phase = 1
	}
	if p.Phase == 1 {
		data, object, err := m.readState(ctx, stateKey(ticket.Identity, ticket.Digest))
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return false, err
		}
		if err == nil {
			var current state
			if json.Unmarshal(data, &current) != nil {
				return false, storage.ErrCorruptObject
			}
			if current.Session == ticket.Session {
				if object.Version != ticket.StateVersion {
					return false, storage.ErrConditionFailed
				}
				if err := guard(ctx); err != nil {
					return false, err
				}
				if err := m.deleteState(ctx, stateKey(ticket.Identity, ticket.Digest), ticket.StateVersion); err != nil && !errors.Is(err, storage.ErrNotFound) {
					return false, err
				}
			}
		}
		p.Phase = 2
		return false, nil
	}
	if err := guard(ctx); err != nil {
		return false, err
	}
	return true, m.release(ctx, ticket.Identity, ticket.Digest, ticket.Session)
}

func (m *Manager) HasState(ctx context.Context, identity, digest string) (bool, error) {
	_, err := m.store.Head(ctx, stateKey(identity, digest))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (m *Manager) CleanupTicketCurrent(ctx context.Context, ticket CleanupTicket) (bool, error) {
	data, object, err := m.readState(ctx, stateKey(ticket.Identity, ticket.Digest))
	if errors.Is(err, storage.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var current state
	if json.Unmarshal(data, &current) != nil {
		return false, storage.ErrCorruptObject
	}
	return current.Session == ticket.Session && object.Version == ticket.StateVersion, nil
}
