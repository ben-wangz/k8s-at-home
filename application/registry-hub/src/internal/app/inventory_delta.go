package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
)

type inventoryDeltaProgress struct {
	Fence     uint64                    `json:"fence,omitempty"`
	Cursor    inventory.Cursor          `json:"cursor"`
	Changes   []inventoryChange         `json:"changes,omitempty"`
	Offset    int                       `json:"offset"`
	Phase     int                       `json:"phase"`
	Fanout    inventory.Cursor          `json:"fanout"`
	Source    *inventoryGraphProjection `json:"source,omitempty"`
	Links     inventoryScan             `json:"links"`
	Projected bool                      `json:"projected,omitempty"`
}

type inventoryDirtyEntity struct {
	Revision string `json:"revision"`
	Captured string `json:"captured,omitempty"`
	Fence    uint64 `json:"fence,omitempty"`
}

func inventoryDirtyKey(id string) string { return inventory.Prefix + "dirty-images/" + id + ".json" }

func (s *Service) advanceInventoryDelta(ctx context.Context, p *inventoryDeltaProgress) (bool, error) {
	switch p.Phase {
	case 0:
		if err := s.captureInventoryChanges(ctx, p); err != nil {
			return false, err
		}
		if p.Cursor.Done || len(p.Changes) == 128 {
			p.Phase = 1
		}
	case 1:
		if p.Offset == len(p.Changes) {
			p.Phase, p.Offset = 2, 0
			return false, nil
		}
		done, err := s.projectInventoryChange(ctx, p)
		if done {
			p.Offset++
			p.Projected = false
		}
		return false, err
	case 2:
		entry, ok, err := p.Fanout.Peek(ctx, s.store, inventory.Prefix+"fragments/image/", 32)
		if err != nil {
			return false, err
		}
		if ok {
			fragment, _, err := inventory.Read[inventoryFragment](ctx, s.store, entry.Key)
			if err != nil && !errors.Is(err, storage.ErrNotFound) {
				return false, err
			}
			if err == nil {
				if err := s.invalidateInventoryFragment(ctx, fragment, p.Changes, p.Fence); err != nil {
					return false, err
				}
			}
			p.Fanout.Advance()
		}
		if p.Fanout.Done {
			p.Phase = 3
		}
	case 3:
		if p.Offset < len(p.Changes) {
			return false, s.acknowledgeInventoryChanges(ctx, p)
		}
		if p.Cursor.Done {
			return true, nil
		}
		p.Phase, p.Offset, p.Changes, p.Fanout = 0, 0, nil, inventory.Cursor{}
	default:
		return false, storage.ErrCorruptObject
	}
	return false, nil
}

func (s *Service) projectInventoryChange(ctx context.Context, p *inventoryDeltaProgress) (bool, error) {
	if len(p.Links.Links) > 0 {
		return false, s.advanceInventoryLinks(ctx, &p.Links)
	}
	if p.Projected {
		return true, nil
	}
	key := p.Changes[p.Offset].Key
	graph := strings.HasPrefix(key, "solidification/graphs/")
	if strings.HasPrefix(key, "tasks/state/") || graph {
		if p.Source == nil {
			var err error
			id := strings.TrimSuffix(strings.TrimPrefix(key, "tasks/state/"), ".json")
			if graph {
				id, _, _ = strings.Cut(strings.TrimPrefix(key, "solidification/graphs/"), "/")
			}
			p.Source, err = s.newInventoryProjection(ctx, id)
			if errors.Is(err, storage.ErrNotFound) {
				return true, nil
			}
			if err == nil && graph {
				p.Source.Complete = false
			}
			// Persist the capture identity before any immutable graph writes.
			return false, err
		}
		if !p.Source.Complete {
			if err := s.advanceInventoryProjection(ctx, p.Source); err != nil {
				return false, err
			}
			return false, nil
		}
		p.Source = nil
	} else if strings.HasPrefix(key, "cache/objects/") && strings.Contains(key, "/representations/") && strings.HasSuffix(key, ".json") {
		links := []inventory.Content{}
		err := s.scanInventoryCacheEntry(context.WithValue(ctx, inventoryLinkCapture{}, &links), key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrCorruptObject) {
			return false, err
		}
		p.Links.Links = links
		p.Projected = true
		if len(links) > 0 {
			return false, nil
		}
	}
	return true, nil
}

func (s *Service) invalidateInventoryFragment(ctx context.Context, fragment inventoryFragment, changes []inventoryChange, fence uint64) error {
	parts := []string{fragment.Image.ID}
	for _, change := range changes {
		version, observed := fragment.Dependencies.Objects[change.Key]
		if observed && version == change.SourceVersion && version != "" {
			continue
		}
		affected := observed
		if strings.HasSuffix(change.Key, "/") {
			for key := range fragment.Dependencies.Objects {
				affected = affected || strings.HasPrefix(key, change.Key)
			}
		}
		for _, listing := range fragment.Dependencies.Listings {
			affected = affected || strings.HasPrefix(change.Key, listing.Prefix) || strings.HasSuffix(change.Key, "/") && strings.HasPrefix(listing.Prefix, change.Key)
		}
		if affected {
			parts = append(parts, change.Key, change.Revision)
		}
	}
	if len(parts) == 1 {
		return nil
	}
	return inventory.Update(ctx, s.store, inventoryDirtyKey(fragment.Image.ID), func(value *inventoryDirtyEntity) error {
		if value.Fence > fence {
			return inventory.ErrSuperseded
		}
		value.Revision = inventory.ID("dirty", parts...)
		value.Fence = fence
		return nil
	})
}
