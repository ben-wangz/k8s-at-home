package logging

import (
	"context"
	"log/slog"
	"sync/atomic"
)

type entry struct {
	handler slog.Handler
	record  slog.Record
	barrier chan struct{}
	stop    bool
}

type state struct {
	queue   chan entry
	dropped atomic.Uint64
}

type Handler struct {
	next  slog.Handler
	state *state
}

func New(next slog.Handler, capacity int) *Handler {
	if capacity < 1 {
		capacity = 1
	}
	current := &state{queue: make(chan entry, capacity)}
	go run(current)
	return &Handler{next: next, state: current}
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(_ context.Context, record slog.Record) error {
	select {
	case h.state.queue <- entry{handler: h.next, record: record.Clone()}:
	default:
		h.state.dropped.Add(1)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), state: h.state}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), state: h.state}
}

func (h *Handler) Dropped() uint64 { return h.state.dropped.Load() }

func (h *Handler) Flush(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case h.state.queue <- entry{barrier: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) Close(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case h.state.queue <- entry{barrier: done, stop: true}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func run(current *state) {
	for item := range current.queue {
		if item.barrier != nil {
			close(item.barrier)
			if item.stop {
				return
			}
			continue
		}
		if err := item.handler.Handle(context.Background(), item.record); err != nil {
			current.dropped.Add(1)
		}
	}
}
