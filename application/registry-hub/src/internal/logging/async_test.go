package logging

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type blockingHandler struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *blockingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *blockingHandler) Handle(context.Context, slog.Record) error {
	h.once.Do(func() { close(h.started) })
	<-h.release
	return nil
}

func (h *blockingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *blockingHandler) WithGroup(string) slog.Handler { return h }

func TestAsyncHandlerDoesNotWaitForSlowSink(t *testing.T) {
	sink := &blockingHandler{started: make(chan struct{}), release: make(chan struct{})}
	handler := New(sink, 1)
	logger := slog.New(handler)
	logger.Info("first")
	select {
	case <-sink.started:
	case <-time.After(time.Second):
		t.Fatal("worker did not start writing")
	}
	logger.Info("queued")
	done := make(chan struct{})
	go func() {
		logger.Info("dropped")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("request path waited for the log sink")
	}
	if handler.Dropped() != 1 {
		t.Fatalf("dropped log count = %d, want 1", handler.Dropped())
	}
	close(sink.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := handler.Close(ctx); err != nil {
		t.Fatalf("close logger: %v", err)
	}
}
