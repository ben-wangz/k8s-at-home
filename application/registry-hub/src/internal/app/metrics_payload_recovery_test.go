package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"registry-hub/internal/storage"
	"strings"
	"testing"
)

type conditionalPayloadStore struct {
	storage.Store
	conditional int
	plain       int
	cause       error
}

func (s *conditionalPayloadStore) GetIfMatch(ctx context.Context, key, version string) (storage.Object, error) {
	if ctx.Err() != nil || storage.IOPurpose(ctx) != "cache_verification" || key != "payload" || version != "expected" {
		return storage.Object{}, storage.ErrCorruptObject
	}
	s.conditional++
	if s.cause != nil {
		return storage.Object{}, s.cause
	}
	return storage.Object{Body: io.NopCloser(strings.NewReader("payload")), Size: 7, Version: version}, nil
}

func (s *conditionalPayloadStore) Get(context.Context, string) (storage.Object, error) {
	s.plain++
	return storage.Object{Body: io.NopCloser(strings.NewReader("payload")), Size: 7, Version: "expected"}, nil
}

func TestProductionWrapperChainPreservesConditionalPayloadReads(t *testing.T) {
	for _, cause := range []error{nil, storage.ErrConditionFailed} {
		source := &conditionalPayloadStore{cause: cause}
		controlled := &recoveryStore{Store: source}
		controlled.blocked.Store(true)
		store := newObservedStore(controlled, newMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
		ctx := storage.WithIOPurpose(context.Background(), "cache_verification")
		object, err := storage.GetIfMatch(ctx, store, "payload", "expected")
		if !errors.Is(err, cause) || source.conditional != 1 || source.plain != 0 {
			t.Fatalf("conditional forwarding error=%v calls=%d plain=%d", err, source.conditional, source.plain)
		}
		if object.Body != nil {
			_ = object.Body.Close()
		}
		_, err = controlled.Put(ctx, "cache/protected", strings.NewReader("x"), 1, storage.PutOptions{})
		if !errors.Is(err, errRecoveryPaused) {
			t.Fatalf("payload forwarding bypassed recovery write barrier: %v", err)
		}
	}
}

func TestProductionWrapperChainRetainsUnsupportedCapabilityFallback(t *testing.T) {
	source := &conditionalPayloadStore{}
	controlled := &recoveryStore{Store: struct{ storage.Store }{source}}
	store := newObservedStore(controlled, newMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if _, err := store.OpenPayload(ctx, "payload"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("unsupported opener=%v", err)
	}
	object, err := storage.GetIfMatch(ctx, store, "payload", "expected")
	if err != nil {
		t.Fatal(err)
	}
	_ = object.Body.Close()
	if source.plain != 1 || source.conditional != 0 {
		t.Fatalf("legacy fallback plain=%d conditional=%d", source.plain, source.conditional)
	}
}
