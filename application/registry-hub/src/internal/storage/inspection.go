package storage

import "context"

type Fingerprinter interface {
	Fingerprint(context.Context, string) (Entry, string, error)
}

func Fingerprint(ctx context.Context, store Store, key string) (string, error) {
	if source, ok := store.(Fingerprinter); ok {
		_, fingerprint, err := source.Fingerprint(ctx, key)
		return fingerprint, err
	}
	return "", nil
}

func Scan(ctx context.Context, store Store, prefix, token string, limit int) ([]Entry, string, error) {
	if source, ok := store.(interface {
		ListMetadata(context.Context, string, string, int) ([]Entry, string, error)
	}); ok {
		return source.ListMetadata(ctx, prefix, token, limit)
	}
	return store.List(ctx, prefix, token, limit)
}
