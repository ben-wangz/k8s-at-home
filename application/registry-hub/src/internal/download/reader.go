package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (m *Manager) Open(ctx context.Context, identity, digest string, maxBytes int64) (io.ReadCloser, int64, error) {
	data, _, err := m.readState(ctx, stateKey(identity, digest))
	if err != nil {
		return nil, 0, err
	}
	value, err := decodeState(data, Request{Identity: identity, Digest: digest, MaxBytes: maxBytes})
	if err != nil {
		return nil, 0, err
	}
	if value.TotalSize < 0 || value.Offset != value.TotalSize || value.Offset > maxBytes {
		return nil, 0, errors.New("download is incomplete")
	}
	items, _, err := m.chunks(ctx, value)
	if err != nil {
		return nil, 0, err
	}
	return &objectReader{ctx: ctx, store: m.store, chunks: items}, value.Offset, nil
}

func (m *Manager) CommitComplete(ctx context.Context, identity, digest string, ownerFence uint64) error {
	key := stateKey(identity, digest)
	data, object, err := m.readState(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	value, err := decodeState(data, Request{Identity: identity, Digest: digest, MaxBytes: int64(^uint64(0) >> 1)})
	if err != nil {
		return err
	}
	if value.OwnerFence != ownerFence || value.TotalSize < 0 || value.Offset != value.TotalSize {
		return storage.ErrLeaseLost
	}
	if err := m.removeSessions(ctx, value); err != nil {
		return err
	}
	if err := m.deleteState(ctx, key, object.Version); err != nil {
		return err
	}
	return m.release(ctx, identity, digest, value.Session)
}

func (m *Manager) Discard(ctx context.Context, identity, digest string, ownerFence uint64) error {
	key := stateKey(identity, digest)
	data, object, err := m.readState(ctx, key)
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	value, err := decodeState(data, Request{Identity: identity, Digest: digest, MaxBytes: int64(^uint64(0) >> 1)})
	if err != nil {
		return err
	}
	if ownerFence != 0 && value.OwnerFence != ownerFence {
		return storage.ErrLeaseLost
	}
	if err := m.removeSessions(ctx, value); err != nil {
		return err
	}
	if err := m.deleteState(ctx, key, object.Version); err != nil {
		return err
	}
	return m.release(ctx, identity, digest, value.Session)
}

func (m *Manager) Cleanup(ctx context.Context, now time.Time, olderThan time.Duration, limit int, acquire func(context.Context, string, string) (context.Context, func(), bool, error)) (int, error) {
	if olderThan <= 0 {
		return 0, errors.New("download retention must be positive")
	}
	if limit < 1 || limit > 10000 {
		limit = 100
	}
	removed := 0
	token := ""
	for removed < limit {
		entries, next, err := m.store.List(ctx, partialPrefix, token, 500)
		if err != nil {
			return removed, err
		}
		for _, entry := range entries {
			if removed >= limit || !strings.HasSuffix(entry.Key, "/state.json") {
				continue
			}
			data, object, err := m.readState(ctx, entry.Key)
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			if err != nil {
				return removed, err
			}
			var value state
			if err := json.Unmarshal(data, &value); err != nil || value.SchemaVersion != stateSchemaVersion || value.Identity == "" || value.Digest == "" {
				return removed, storage.ErrCorruptObject
			}
			if now.Sub(value.ProgressAt) < olderThan {
				continue
			}
			leaseCtx, release, acquired, err := acquire(ctx, value.Identity, value.Digest)
			if err != nil {
				return removed, err
			}
			if !acquired {
				continue
			}
			currentData, currentObject, err := m.readState(leaseCtx, entry.Key)
			if errors.Is(err, storage.ErrNotFound) || err == nil && currentObject.Version != object.Version {
				release()
				continue
			}
			if err != nil {
				release()
				return removed, err
			}
			current, err := decodeState(currentData, Request{Identity: value.Identity, Digest: value.Digest, MaxBytes: int64(^uint64(0) >> 1)})
			if err != nil || now.Sub(current.ProgressAt) < olderThan {
				release()
				if err != nil {
					return removed, err
				}
				continue
			}
			if err := m.removeSessions(leaseCtx, current); err != nil {
				release()
				return removed, err
			}
			if err := m.deleteState(leaseCtx, entry.Key, currentObject.Version); err != nil {
				release()
				if errors.Is(err, storage.ErrConditionFailed) || errors.Is(err, storage.ErrNotFound) {
					continue
				}
				return removed, err
			}
			err = m.release(leaseCtx, current.Identity, current.Digest, current.Session)
			release()
			if err != nil {
				return removed, err
			}
			removed++
		}
		if next == "" {
			break
		}
		token = next
	}
	if err := m.cleanupOrphanReservations(ctx); err != nil {
		return removed, err
	}
	return removed, nil
}

type objectReader struct {
	ctx     context.Context
	store   storage.Store
	chunks  []chunk
	index   int
	current *bytes.Reader
}

func (r *objectReader) Read(buffer []byte) (int, error) {
	for r.current == nil || r.current.Len() == 0 {
		if r.index >= len(r.chunks) {
			return 0, io.EOF
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		current, err := readVerifiedChunk(r.ctx, r.store, r.chunks[r.index])
		if err != nil {
			return 0, err
		}
		r.current = bytes.NewReader(current)
		r.index++
	}
	return r.current.Read(buffer)
}

func (r *objectReader) Close() error { return nil }

func readVerifiedChunk(ctx context.Context, store storage.Store, item chunk) ([]byte, error) {
	object, err := store.Get(ctx, item.Key)
	if err != nil {
		return nil, err
	}
	defer object.Body.Close()
	if object.Version != item.Version || object.Size != item.Size {
		return nil, fmt.Errorf("%w: download chunk metadata changed", storage.ErrCorruptObject)
	}
	data, err := io.ReadAll(io.LimitReader(object.Body, item.Size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != item.Size {
		return nil, storage.ErrCorruptObject
	}
	hash := sha256.Sum256(data)
	if hex.EncodeToString(hash[:]) != item.Hash {
		return nil, storage.ErrCorruptObject
	}
	return data, nil
}
