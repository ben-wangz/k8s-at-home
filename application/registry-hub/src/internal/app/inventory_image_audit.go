package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (s *Service) advanceInventoryImageAudit(ctx context.Context, p *inventoryImageProgress) error {
	done, err := s.advanceInventoryAudit(ctx, p.Audit)
	if err != nil && !errors.Is(err, storage.ErrCorruptObject) && !errors.Is(err, storage.ErrConditionFailed) {
		return err
	}
	if !done && err == nil {
		return nil
	}
	object := p.Audit.Object
	object.Associated = true
	object.DistributionVerified = done && err == nil
	if err != nil {
		object.Reasons = appendReason(object.Reasons, "local_evidence_stale")
	}
	p.Objects = append(p.Objects, object)
	for _, reason := range object.Reasons {
		p.Image.Evidence.Reasons = appendReason(p.Image.Evidence.Reasons, reason)
	}
	p.Audit = nil
	p.Offset++
	return nil
}
