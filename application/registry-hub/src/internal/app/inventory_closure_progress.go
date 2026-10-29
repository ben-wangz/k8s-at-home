package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type inventoryClosureFrame struct {
	Digest   string               `json:"digest"`
	Content  *inventory.Content   `json:"content,omitempty"`
	Child    int                  `json:"child"`
	Expected *inventory.ObjectRef `json:"expected,omitempty"`
}

type inventoryClosureProgress struct {
	Objects      []inventory.ObjectRef          `json:"objects"`
	Platforms    []string                       `json:"platforms"`
	Required     map[string]inventory.ObjectRef `json:"required"`
	RequiredRefs []inventory.ObjectRef          `json:"requiredRefs"`
	Seen         map[string]inventory.ObjectRef `json:"seen"`
	Active       map[string]bool                `json:"active"`
	Frames       []inventoryClosureFrame        `json:"frames"`
	Complete     bool                           `json:"complete"`
	Bytes        int64                          `json:"bytes"`
	Reasons      []string                       `json:"reasons"`
}

func newInventoryClosureProgress(digest string) inventoryClosureProgress {
	return inventoryClosureProgress{Objects: []inventory.ObjectRef{}, Platforms: []string{}, Required: map[string]inventory.ObjectRef{}, Seen: map[string]inventory.ObjectRef{}, Active: map[string]bool{}, Frames: []inventoryClosureFrame{{Digest: digest}}, Complete: true, Reasons: []string{}}
}

func (p *inventoryClosureProgress) incomplete(reason string) {
	p.Complete = false
	p.Reasons = appendReason(p.Reasons, reason)
}

func (p *inventoryClosureProgress) pop() {
	delete(p.Active, p.Frames[len(p.Frames)-1].Digest)
	p.Frames = p.Frames[:len(p.Frames)-1]
}

func (s *Service) advanceInventoryClosure(ctx context.Context, identity inventory.Identity, p *inventoryClosureProgress) error {
	if len(p.Frames) == 0 {
		return nil
	}
	f := &p.Frames[len(p.Frames)-1]
	if f.Content == nil {
		return s.enterInventoryClosure(ctx, identity, p)
	}
	if f.Child == len(f.Content.Children) {
		p.pop()
		return nil
	}
	child := f.Content.Children[f.Child]
	f.Child++
	key := "blob:" + child.Digest
	if child.Role == "manifest" {
		key = "manifest:" + child.Digest
		if _, exists := p.Seen[key]; !exists {
			if _, exists := p.Required[child.Digest]; !exists {
				p.Required[child.Digest] = child
				p.RequiredRefs = append(p.RequiredRefs, child)
				p.Platforms = append(p.Platforms, inventory.ID("platform", identity.Authority, identity.Repository, child.Digest))
			}
		}
		if p.Active[child.Digest] || len(p.Frames) > s.cfg.Limits.ClosureDepth {
			p.incomplete("closure_limit_or_cycle")
			return nil
		}
		p.Frames = append(p.Frames, inventoryClosureFrame{Digest: child.Digest, Expected: &child})
		return nil
	}
	if old, exists := p.Seen[key]; exists {
		if old.Size != child.Size {
			p.incomplete("descriptor_conflict")
		}
		return nil
	}
	if len(p.Objects) >= s.cfg.Limits.ClosureObjects || child.Size < 0 || child.Size > s.cfg.Limits.ClosureBytes-p.Bytes {
		p.incomplete("closure_limit")
		return nil
	}
	p.Objects = append(p.Objects, child)
	p.Seen[key], p.Bytes = child, p.Bytes+child.Size
	return nil
}

func (s *Service) enterInventoryClosure(ctx context.Context, identity inventory.Identity, p *inventoryClosureProgress) error {
	f := &p.Frames[len(p.Frames)-1]
	content, err := s.inventory.Content(ctx, identity, f.Digest)
	if errors.Is(err, storage.ErrNotFound) {
		p.incomplete("closure_incomplete")
		p.pop()
		return nil
	}
	if err != nil {
		return err
	}
	if f.Expected != nil && content.ClosureKnown && (content.Size != f.Expected.Size || content.MediaType != f.Expected.MediaType) {
		p.incomplete("descriptor_conflict")
	}
	if _, exists := p.Seen["manifest:"+f.Digest]; exists {
		p.pop()
		return nil
	}
	if len(p.Objects) >= s.cfg.Limits.ClosureObjects || content.Size < 0 || content.Size > s.cfg.Limits.ClosureBytes-p.Bytes {
		p.incomplete("closure_limit")
		p.pop()
		return nil
	}
	p.Seen["manifest:"+f.Digest], p.Active[f.Digest] = content.ObjectRef, true
	p.Objects = append(p.Objects, content.ObjectRef)
	p.Bytes += content.Size
	if !content.ClosureKnown {
		p.incomplete("closure_incomplete")
	}
	f.Content = &content
	return nil
}
