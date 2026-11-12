package storage

import (
	"context"
	"errors"
	"io"
)

type PayloadReader interface {
	io.ReadSeekCloser
	Check(context.Context) error
}

type Payload struct {
	Body        PayloadReader
	Size        int64
	Fingerprint string
}

// PayloadOpener avoids computing a storage version before cache verification.
type PayloadOpener interface {
	OpenPayload(context.Context, string) (Payload, error)
}

type ConditionalGetter interface {
	GetIfMatch(context.Context, string, string) (Object, error)
}

func GetIfMatch(ctx context.Context, store Store, key, version string) (Object, error) {
	if source, ok := store.(ConditionalGetter); ok {
		object, err := source.GetIfMatch(ctx, key, version)
		if !errors.Is(err, errors.ErrUnsupported) {
			return object, err
		}
	}
	return store.Get(ctx, key)
}
