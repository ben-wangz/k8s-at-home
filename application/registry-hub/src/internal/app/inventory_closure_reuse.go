package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"slices"
	"strconv"
)

type inventoryClosureReuse struct {
	Closure inventoryClosureProgress `json:"closure"`
	Digests []string                 `json:"digests"`
	Offset  int                      `json:"offset"`
}

func closureReuseKey(image inventory.Image) string {
	return inventory.Prefix + "closure-topology/" + inventory.ID(image.ContentID, strconv.FormatUint(image.ContentGeneration, 10)) + ".json"
}

func (s *Service) loadClosureReuse(ctx context.Context, p *inventoryImageProgress) error {
	cached, _, err := inventory.Read[inventoryClosureProgress](ctx, s.store, closureReuseKey(p.Image))
	if errors.Is(err, storage.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !cached.Complete || len(cached.SourceVersions) == 0 || len(cached.Frames) != 0 || len(cached.Objects) > 1 && cached.Parents == nil {
		return nil
	}
	if len(cached.Objects) > s.cfg.Limits.ClosureObjects || cached.Bytes > s.cfg.Limits.ClosureBytes || cached.Depth > s.cfg.Limits.ClosureDepth+1 {
		return nil
	}
	digests := make([]string, 0, len(cached.SourceVersions))
	for digest := range cached.SourceVersions {
		digests = append(digests, digest)
	}
	slices.Sort(digests)
	p.ClosureReuse = &inventoryClosureReuse{Closure: cached, Digests: digests}
	return nil
}

func (s *Service) advanceClosureReuse(ctx context.Context, p *inventoryImageProgress) error {
	candidate := p.ClosureReuse
	if candidate.Offset < len(candidate.Digests) {
		digest := candidate.Digests[candidate.Offset]
		entry, err := s.store.Head(ctx, inventory.Prefix+"contents/"+p.Image.Identity.ContentID(digest)+".json")
		if errors.Is(err, storage.ErrNotFound) || err == nil && (entry.Version == "" || entry.Version != candidate.Closure.SourceVersions[digest]) {
			p.ClosureReuse = nil
			return nil
		}
		if err != nil {
			return err
		}
		candidate.Offset++
		return nil
	}
	p.Closure, p.ClosureReuse, p.TopologySaved = candidate.Closure, nil, true
	return nil
}

func (s *Service) saveClosureReuse(ctx context.Context, p *inventoryImageProgress) error {
	if p.TopologySaved || !p.Closure.Complete || len(p.Closure.SourceVersions) == 0 {
		return nil
	}
	value := p.Closure
	value.Seen, value.Active, value.Required = nil, nil, nil
	err := inventory.Update(ctx, s.store, closureReuseKey(p.Image), func(current *inventoryClosureProgress) error { *current = value; return nil })
	if err == nil {
		p.TopologySaved = true
	}
	return err
}
