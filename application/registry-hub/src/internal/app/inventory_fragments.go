package app

import (
	"context"
	"encoding/json"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type inventoryFragment struct {
	Image         inventory.Image          `json:"image"`
	Objects       []inventory.Object       `json:"objects"`
	Closure       inventoryClosureProgress `json:"closure"`
	Dependencies  inventoryDependencies    `json:"dependencies"`
	Epoch         uint64                   `json:"epoch"`
	CapturedAt    time.Time                `json:"capturedAt"`
	DirtyRevision string                   `json:"dirtyRevision,omitempty"`
	Fence         uint64                   `json:"fence,omitempty"`
}

type inventoryFragmentReuse struct {
	Fragment inventoryFragment `json:"fragment"`
	Kind     string            `json:"kind"`
	Objects  []string          `json:"objects"`
	Listings []string          `json:"listings"`
	Offset   int               `json:"offset"`
}

func inventoryFragmentKey(image inventory.Image, kind string) string {
	id := image.ID
	if kind == "content" {
		id = inventory.ID("content-fragment", image.ContentID, strconv.FormatUint(image.ContentGeneration, 10))
	}
	return inventory.Prefix + "fragments/" + kind + "/" + id + ".json"
}

func (s *Service) loadInventoryFragment(ctx context.Context, p *inventoryImageProgress) error {
	for _, kind := range []string{"image", "content"} {
		value, _, err := inventory.Read[inventoryFragment](ctx, s.store, inventoryFragmentKey(p.Image, kind))
		if errors.Is(err, storage.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if value.Epoch != p.Epoch || value.Dependencies.Invalid || len(value.Dependencies.Objects) == 0 || !value.Closure.Complete || value.Image.ContentID != p.Image.ContentID || value.Image.ResolvedDigest != p.Image.ResolvedDigest || value.Image.ContentGeneration != p.Image.ContentGeneration {
			continue
		}
		if len(value.Objects) > s.cfg.Limits.ClosureObjects || value.Closure.Bytes > s.cfg.Limits.ClosureBytes || value.Closure.Depth > s.cfg.Limits.ClosureDepth+1 || time.Since(value.CapturedAt) > min(s.inventoryConfig().StaleAfter, max(5*time.Minute, s.inventoryConfig().RefreshInterval)) {
			continue
		}
		if kind == "image" && (value.Image.ID != p.Image.ID || value.Image.BindingRevision != p.Image.BindingRevision || value.Image.Generation != p.Image.Generation || value.DirtyRevision != p.DirtyRevision) {
			continue
		}
		candidate := &inventoryFragmentReuse{Fragment: value, Kind: kind}
		for key := range value.Dependencies.Objects {
			candidate.Objects = append(candidate.Objects, key)
		}
		for key := range value.Dependencies.Listings {
			candidate.Listings = append(candidate.Listings, key)
		}
		slices.Sort(candidate.Objects)
		slices.Sort(candidate.Listings)
		p.Reuse = candidate
		return nil
	}
	return nil
}

func (s *Service) advanceInventoryFragment(ctx context.Context, p *inventoryImageProgress) error {
	candidate := p.Reuse
	d := &candidate.Fragment.Dependencies
	valid := true
	if candidate.Offset < len(candidate.Objects) {
		end := min(candidate.Offset+4, len(candidate.Objects))
		matched, err := s.validateInventoryDependencies(ctx, d, candidate.Objects[candidate.Offset:end])
		if err != nil {
			return err
		}
		if matched {
			candidate.Offset = end
			return nil
		}
		valid = false
	} else if offset := candidate.Offset - len(candidate.Objects); offset < len(candidate.Listings) {
		listing := d.Listings[candidate.Listings[offset]]
		entries, next, err := s.inventoryDependencyListing(ctx, listing)
		if err != nil {
			return err
		}
		valid = listingFingerprint(entries, next) == listing.Fingerprint
	} else {
		gate, err := s.inventoryEpoch(ctx)
		if err != nil {
			return err
		}
		if gate.Epoch == p.Epoch && !gate.Exclusive && !gate.RecoveryBarrier && gate.State == "open" {
			fragment := candidate.Fragment
			fragment.Dependencies.merge(p.Dependencies)
			p.Objects, p.Closure, p.Dependencies = fragment.Objects, fragment.Closure, fragment.Dependencies
			p.Anchored = false
			if candidate.Kind == "image" {
				p.Image = fragment.Image
				p.Phase = 6
			} else {
				for index := range p.Objects {
					p.Objects[index].Identity = p.Image.Identity
				}
				p.Image.Cache, p.Image.Availability, p.Anchored = summarizeInventoryObjects(p.Objects, p.Closure.Complete)
				p.Image.Evidence = fragment.Image.Evidence
				p.Phase = 2
			}
			p.Reuse = nil
			return nil
		}
		valid = false
	}
	if !valid {
		p.Reuse = nil
		return s.loadClosureReuse(ctx, p)
	}
	candidate.Offset++
	return nil
}

func (s *Service) inventoryDependencyListing(ctx context.Context, listing inventoryListing) ([]storage.Entry, string, error) {
	if listing.Metadata {
		return storage.Scan(ctx, s.store, listing.Prefix, listing.Token, listing.Limit)
	}
	return s.store.List(ctx, listing.Prefix, listing.Token, listing.Limit)
}

func (s *Service) validateInventoryDependencies(ctx context.Context, d *inventoryDependencies, keys []string) (bool, error) {
	matched, failures := make([]bool, len(keys)), make([]error, len(keys))
	var workers sync.WaitGroup
	for index, key := range keys {
		workers.Go(func() {
			version, err := s.inventoryDependencyVersion(ctx, key, d.Objects[key])
			if errors.Is(err, storage.ErrNotFound) {
				version, err = "missing", nil
			}
			matched[index], failures[index] = version != "" && version == d.Objects[key], err
		})
	}
	workers.Wait()
	valid := true
	for _, match := range matched {
		valid = valid && match
	}
	return valid, errors.Join(failures...)
}

func (s *Service) inventoryDependencyVersion(ctx context.Context, key, expected string) (string, error) {
	if strings.HasPrefix(expected, "fingerprint:") {
		value, err := storage.Fingerprint(ctx, s.store, key)
		return "fingerprint:" + value, err
	}
	entry, err := s.store.Head(ctx, key)
	return entry.Version, err
}

func (s *Service) saveInventoryFragment(ctx context.Context, p *inventoryImageProgress, kind string) error {
	if p.Dependencies.Invalid || len(p.Dependencies.Objects) == 0 || !p.Closure.Complete || p.Invalid || p.Image.Evidence.Freshness != "fresh" {
		return nil
	}
	fragment := inventoryFragment{Image: p.Image, Objects: p.Objects, Closure: p.Closure, Dependencies: p.Dependencies, Epoch: p.Epoch, CapturedAt: p.Image.Evidence.CheckedAt, DirtyRevision: p.DirtyRevision, Fence: p.Fence}
	data, err := json.Marshal(fragment)
	if err != nil {
		return err
	}
	if len(data) > 8<<20 {
		return nil
	}
	return inventory.Update(ctx, s.store, inventoryFragmentKey(p.Image, kind), func(value *inventoryFragment) error {
		if value.Fence > fragment.Fence {
			return inventory.ErrSuperseded
		}
		if !value.CapturedAt.After(fragment.CapturedAt) {
			*value = fragment
		}
		return nil
	})
}
