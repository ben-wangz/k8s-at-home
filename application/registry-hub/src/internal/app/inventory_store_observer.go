package app

import (
	"context"
	"registry-hub/internal/storage"
	"strings"
	"time"
)

func (s *observedStore) Fingerprint(ctx context.Context, key string) (storage.Entry, string, error) {
	if source, ok := s.Store.(storage.Fingerprinter); ok {
		entry, fingerprint, err := source.Fingerprint(ctx, key)
		if fingerprint != "" || err != nil {
			recordInventoryDependency(ctx, key, "fingerprint:"+fingerprint, err)
		}
		return entry, fingerprint, err
	}
	return storage.Entry{}, "", nil
}

func (s *observedStore) ListMetadata(ctx context.Context, prefix, token string, limit int) ([]storage.Entry, string, error) {
	started := time.Now()
	entries, next, err := storage.Scan(ctx, s.Store, prefix, token, limit)
	recordInventoryListing(ctx, prefix, token, limit, entries, next, err, true)
	s.record(ctx, "list", err, started)
	return entries, next, err
}

func inventorySourceKey(key string) bool {
	return strings.HasPrefix(key, "cache/objects/") || strings.HasPrefix(key, "tasks/") || strings.HasPrefix(key, "solidification/") || strings.HasPrefix(key, "generations/") || strings.HasPrefix(key, "system/gc/")
}
