package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

type refreshingExpiredReaderPinStore struct {
	storage.Store
	refreshed bool
}

func (s *refreshingExpiredReaderPinStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	if strings.HasPrefix(key, "cache/readers/") && !s.refreshed {
		s.refreshed = true
		object, err := s.Store.Get(ctx, key)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(object.Body)
		closeErr := object.Body.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		var pin cacheReaderPinRecord
		if err := json.Unmarshal(data, &pin); err != nil {
			return err
		}
		pin.ExpiresAt = time.Now().UTC().Add(time.Hour)
		refreshed, err := json.Marshal(pin)
		if err != nil {
			return err
		}
		if _, err := s.Store.Put(ctx, key, bytes.NewReader(refreshed), int64(len(refreshed)), storage.PutOptions{IfMatch: object.Version}); err != nil {
			return err
		}
		return storage.ErrConditionFailed
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestCacheCleanupTreatsReaderPinRenewalConflictAsActive(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	store := &refreshingExpiredReaderPinStore{Store: base}
	cache := NewCache(store)
	ctx := context.Background()
	body := []byte("reader pin renewal race")
	const authority, repository, mediaType = "ghcr.io", "org/image", "application/octet-stream"
	digest := CalculateSHA256(body)
	if err := cache.Put(ctx, authority, repository, digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := cache.MarkCleanupPending(ctx, authority, repository, []string{digest}, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	reader, err := cache.GetAnyRetained(ctx, authority, repository, digest)
	if err != nil {
		t.Fatalf("open retained cache reader: %v", err)
	}
	defer reader.Body.Close()

	identity := cacheIdentity(authority, repository, digest)
	pins, _, err := base.List(ctx, "cache/readers/"+identity+"/", "", 10)
	if err != nil || len(pins) != 1 {
		t.Fatalf("reader pins = %d, err=%v", len(pins), err)
	}
	object, err := base.Get(ctx, pins[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	pinBytes, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read reader pin: %v", errors.Join(readErr, closeErr))
	}
	var pin cacheReaderPinRecord
	if err := json.Unmarshal(pinBytes, &pin); err != nil {
		t.Fatal(err)
	}
	pin.ExpiresAt = time.Now().Add(-time.Second)
	expired, err := json.Marshal(pin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Put(ctx, pins[0].Key, bytes.NewReader(expired), int64(len(expired)), storage.PutOptions{IfMatch: object.Version}); err != nil {
		t.Fatal(err)
	}

	removed, err := cache.Cleanup(ctx, time.Now().UTC(), 100)
	if err != nil {
		t.Fatalf("cleanup cache: %v", err)
	}
	if removed != 0 || !store.refreshed {
		t.Fatalf("cleanup removed=%d refreshed=%t, want active reader pin to block deletion", removed, store.refreshed)
	}
	retained, err := cache.GetAnyRetained(ctx, authority, repository, digest)
	if err != nil {
		t.Fatalf("cache object was removed after reader pin renewal: %v", err)
	}
	data, readErr := io.ReadAll(retained.Body)
	closeErr = retained.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(data, body) {
		t.Fatalf("retained cache data = %q, read error=%v close error=%v", data, readErr, closeErr)
	}
}
