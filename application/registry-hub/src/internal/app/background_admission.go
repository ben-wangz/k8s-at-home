package app

import (
	"context"
	"strings"
)

func (s *Service) backgroundBudget(ctx context.Context) (func(), bool) {
	return s.backgroundKindBudget(ctx, "")
}

func (s *Service) backgroundKindBudget(ctx context.Context, kind string) (func(), bool) {
	releaseCleanup, admitted := s.acquireRetentionCleanupBudget(ctx, kind)
	if !admitted {
		return nil, false
	}
	releaseBackground, admitted := s.acquireBackgroundCapacity(ctx, kind)
	if !admitted {
		releaseCleanup()
		return nil, false
	}
	return func() {
		releaseBackground()
		releaseCleanup()
	}, true
}

func (s *Service) acquireBackgroundCapacity(ctx context.Context, kind string) (func(), bool) {
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

func (s *Service) acquireRetentionCleanupBudget(ctx context.Context, kind string) (func(), bool) {
	if kind != historyCleanupWorkKind && kind != "receipt_cleanup" && kind != "receipt_cleanup_preview" &&
		kind != "retry_request_retention" && !strings.HasPrefix(kind, "diagnostic_retention_") {
		return func() {}, ctx.Err() == nil
	}
	s.retentionCleanupOnce.Do(func() { s.retentionCleanupSlots = make(chan struct{}, 1) })
	select {
	case s.retentionCleanupSlots <- struct{}{}:
		return func() { <-s.retentionCleanupSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func queueIdleContext(ctx context.Context, idle bool) context.Context {
	return context.WithValue(ctx, backgroundIdleKey{}, idle)
}
