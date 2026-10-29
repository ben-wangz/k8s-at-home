package app

import (
	"context"
	"errors"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
)

func inventorySourcePage(state *inventoryScan) *inventory.Cursor {
	if state.Page == nil {
		state.Page = &inventory.Cursor{Token: state.Token}
	}
	return state.Page
}

func finishInventorySourcePage(state *inventoryScan) {
	state.Token = state.Page.Token
	if state.Page.Done {
		state.Phase++
		state.Token, state.Page = "", nil
	}
}

func (s *Service) scanInventoryTasks(ctx context.Context, state *inventoryScan) error {
	if state.Root != nil {
		err := s.advanceInventoryProjection(ctx, state.Root)
		if errors.Is(err, storage.ErrNotFound) {
			_, missing := s.store.Head(ctx, "tasks/state/"+state.Root.Task.ID+".json")
			if errors.Is(missing, storage.ErrNotFound) {
				state.Root.Cursor, state.Root.Phase = inventory.Cursor{}, 4
				err = nil
			}
		}
		if err != nil {
			return err
		}
		if state.Root.Complete {
			state.Root = nil
			state.Page.Advance()
			finishInventorySourcePage(state)
		}
		return nil
	}
	page := inventorySourcePage(state)
	entry, ok, err := page.Peek(ctx, s.store, "tasks/state/", s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	if ok {
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Key, "tasks/state/"), ".json")
		p, err := s.newInventoryProjection(ctx, id)
		if errors.Is(err, storage.ErrNotFound) {
			page.Advance()
		} else if err != nil {
			return err
		} else {
			state.Root = p
		}
	}
	finishInventorySourcePage(state)
	return nil
}

type inventoryLinkCapture struct{}

func (s *Service) scanInventoryEntry(ctx context.Context, state *inventoryScan, prefix string, process func(context.Context, string) error) error {
	page := inventorySourcePage(state)
	entry, ok, err := page.Peek(ctx, s.store, prefix, s.inventoryConfig().BatchSize)
	if err != nil {
		return err
	}
	if ok {
		links := []inventory.Content{}
		err := process(context.WithValue(ctx, inventoryLinkCapture{}, &links), entry.Key)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return err
		}
		state.Links = links
		page.Advance()
	}
	finishInventorySourcePage(state)
	return nil
}

func (s *Service) advanceInventoryLinks(ctx context.Context, state *inventoryScan) error {
	content := state.Links[0]
	refs := append([]inventory.ObjectRef{content.ObjectRef}, content.Children...)
	if state.LinkOffset < len(refs) {
		child := refs[state.LinkOffset]
		content.Children = nil
		if state.LinkOffset > 0 {
			content.Children = []inventory.ObjectRef{child}
		}
		if err := s.linkInventoryContent(ctx, content); err != nil {
			return err
		}
		if state.LinkOffset > 0 {
			if err := s.inventoryChildDescriptors(ctx, content); err != nil {
				return err
			}
		}
		state.LinkOffset++
	}
	if state.LinkOffset == len(refs) {
		state.Links, state.LinkOffset = state.Links[1:], 0
	}
	return nil
}
