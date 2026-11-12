package app

import (
	"context"
)

func (s *Service) backgroundBudget(ctx context.Context) (func(), bool) {
	return s.backgroundKindBudget(ctx, "")
}

func (s *Service) backgroundKindBudget(ctx context.Context, kind string) (func(), bool) {
	slots, pace := s.backgroundSlots, s.backgroundPace
	if kind == "storage_inventory" {
		slots, pace = s.collectorSlots, s.collectorPace
	}
	if ctx.Err() != nil {
		return nil, false
	}
	if s.backgroundScheduler != nil && kind != "storage_inventory" {
		release, admitted := s.backgroundScheduler.acquire(ctx, kind, urgentBackgroundKind(ctx, kind), s.backgroundReady[kind])
		if !admitted {
			return nil, false
		}
		if pace != nil {
			select {
			case <-ctx.Done():
				release()
				return nil, false
			case <-pace:
			}
		}
		return release, true
	}
	if pace != nil {
		select {
		case <-ctx.Done():
			return nil, false
		case <-pace:
		}
	}
	if slots == nil {
		return func() {}, true
	}
	select {
	case slots <- struct{}{}:
		if ctx.Err() != nil {
			<-slots
			return nil, false
		}
		return func() { <-slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func queueIdleContext(ctx context.Context, idle bool) context.Context {
	return context.WithValue(ctx, backgroundIdleKey{}, idle)
}
