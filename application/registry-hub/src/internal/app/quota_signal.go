package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"time"
)

const quotaSignalKey = "background/v1/quota-signal.json"

type quotaSignal struct {
	Revision uint64 `json:"revision"`
}

func (s *Service) refreshQuotaSignal(ctx context.Context, p *sourceSweepCheckpoint) error {
	signal, _, err := inventory.Read[quotaSignal](ctx, s.store, quotaSignalKey)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	gate := s.gate.State()
	data, _ := json.Marshal([]any{gate.Epoch, gate.State, gate.RecoveryBarrier, gate.ReservedDistributionBytes, gate.NewProtectionSessionCount, gate.ProtectedCacheBytes})
	state := string(data)
	if signal.Revision != p.QuotaSignalRevision || state != p.QuotaGateState {
		p.NextRound = time.Time{}
		if p.Cursor.Entities == 0 && len(p.QuotaCandidates) == 0 {
			p.QuotaSignalRevision, p.QuotaGateState = signal.Revision, state
		}
	}
	return nil
}
