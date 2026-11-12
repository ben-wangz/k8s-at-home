package app

import (
	"context"
	"errors"
	"registry-hub/internal/storage"
)

func (s *recoveryStore) OpenPayload(ctx context.Context, key string) (storage.Payload, error) {
	if source, ok := s.Store.(storage.PayloadOpener); ok {
		return source.OpenPayload(ctx, key)
	}
	return storage.Payload{}, errors.ErrUnsupported
}

func (s *recoveryStore) GetIfMatch(ctx context.Context, key, version string) (storage.Object, error) {
	if source, ok := s.Store.(storage.ConditionalGetter); ok {
		return source.GetIfMatch(ctx, key, version)
	}
	return storage.Object{}, errors.ErrUnsupported
}
