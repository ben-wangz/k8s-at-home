package app

import (
	"context"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

type inventoryAuditCapture struct{}

type inventoryAuditProgress struct {
	Object inventory.Object `json:"object"`
	Path   string           `json:"path"`
	Epoch  uint64           `json:"epoch"`
	Offset int64            `json:"offset"`
	Hash   []byte           `json:"hash,omitempty"`
}

func inventoryAuditHasher(p *inventoryAuditProgress) (hash.Hash, error) {
	h, err := artifact.Hasher(p.Object.Digest)
	if err != nil {
		return nil, err
	}
	if len(p.Hash) > 0 {
		state, ok := h.(encoding.BinaryUnmarshaler)
		if !ok {
			return nil, storage.ErrCorruptObject
		}
		if err := state.UnmarshalBinary(p.Hash); err != nil {
			return nil, storage.ErrCorruptObject
		}
	}
	return h, nil
}

func (s *Service) advanceInventoryAudit(ctx context.Context, p *inventoryAuditProgress) (bool, error) {
	gate, err := s.inventoryEpoch(ctx)
	if err != nil {
		return false, err
	}
	generation, err := s.generation(ctx, p.Object.Authority, p.Object.Repository, p.Object.Digest)
	if err != nil {
		return false, err
	}
	if gate.Epoch != p.Epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier || generation.Tombstone || generation.Generation != p.Object.Generation {
		return false, storage.ErrConditionFailed
	}
	h, err := inventoryAuditHasher(p)
	if err != nil {
		return false, err
	}
	if p.Offset < p.Object.Size || p.Offset == 0 && p.Object.Size == 0 {
		if err := s.readInventoryAuditChunk(ctx, p, h); err != nil {
			return false, err
		}
	}
	if p.Offset != p.Object.Size {
		return false, nil
	}
	digest := strings.SplitN(p.Object.Digest, ":", 2)[0] + ":" + hex.EncodeToString(h.Sum(nil))
	if digest != p.Object.Digest {
		return false, storage.ErrCorruptObject
	}
	gate, err = s.inventoryEpoch(ctx)
	if err != nil {
		return false, err
	}
	generation, err = s.generation(ctx, p.Object.Authority, p.Object.Repository, p.Object.Digest)
	if err != nil {
		return false, err
	}
	if gate.Epoch != p.Epoch || gate.Exclusive || gate.State != "open" || gate.RecoveryBarrier || generation.Tombstone || generation.Generation != p.Object.Generation {
		return false, storage.ErrConditionFailed
	}
	err = s.saveInventoryProof(ctx, p.Object.ID, inventoryProof{SchemaVersion: 1, Generation: p.Object.Generation, Epoch: p.Epoch, State: "verified", ObservedAt: time.Now().UTC()})
	return err == nil, err
}

func (s *Service) readInventoryAuditChunk(ctx context.Context, p *inventoryAuditProgress, h hash.Hash) error {
	length := min(int64(4<<20), p.Object.Size-p.Offset)
	headers := manifestReadHeaders()
	if length > 0 {
		headers.Set("Range", fmt.Sprintf("bytes=%d-%d", p.Offset, p.Offset+length-1))
	}
	response, err := s.registry.Do(ctx, http.MethodGet, p.Path, nil, headers, nil, 0)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	expectedRange := fmt.Sprintf("bytes %d-%d/%d", p.Offset, p.Offset+length-1, p.Object.Size)
	full := p.Offset == 0 && length == p.Object.Size && response.StatusCode == http.StatusOK
	ranged := response.StatusCode == http.StatusPartialContent && response.Header.Get("Content-Range") == expectedRange
	if !full && !ranged || response.ContentLength != length || response.Header.Get("Docker-Content-Digest") != p.Object.Digest {
		return storage.ErrCorruptObject
	}
	size, err := io.Copy(h, io.LimitReader(response.Body, length+1))
	if err != nil {
		return err
	}
	if size != length {
		return storage.ErrCorruptObject
	}
	state, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return storage.ErrCorruptObject
	}
	encoded, err := state.MarshalBinary()
	if err != nil {
		return err
	}
	p.Offset, p.Hash = p.Offset+length, encoded
	return nil
}
