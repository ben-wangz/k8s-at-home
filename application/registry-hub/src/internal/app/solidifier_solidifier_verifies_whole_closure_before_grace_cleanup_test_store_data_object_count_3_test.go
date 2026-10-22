package app

import (
	"context"
	"fmt"
	"registry-hub/internal/storage"
	"strings"
	"testing"
)

func storeDataObjectCount(t *testing.T, ctx context.Context, store storage.Store, want int) error {
	t.Helper()
	cacheEntries, _, err := store.List(ctx, "cache/objects/", "", 100)
	if err != nil {
		return err
	}
	dataEntries := 0
	dataKeys := make([]string, 0)
	for _, entry := range cacheEntries {
		if strings.HasSuffix(entry.Key, "/data") {
			dataEntries++
			dataKeys = append(dataKeys, entry.Key)
		}
	}
	if dataEntries != want {
		return fmt.Errorf("cache data objects = %d, want %d: %v", dataEntries, want, dataKeys)
	}
	return nil
}
