package artifact

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
)

func TestCacheCommitBoundariesRecoverWithoutRewritingPayload(t *testing.T) {
	for _, stage := range []string{"data", "metadata", "index", "complete"} {
		for _, committed := range []bool{false, true} {
			name := stage + "/before_commit"
			if committed {
				name = stage + "/after_commit"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				store, err := filesystem.New(root)
				if err != nil {
					t.Fatal(err)
				}
				body := []byte("cache commit recovery payload")
				digest := CalculateSHA256(body)
				identity := cacheIdentity("ghcr.io", "team/image", digest)
				dataKey := "cache/objects/" + identity + "/data"
				mediaType := "application/octet-stream"
				keys := map[string]string{"data": dataKey, "metadata": cacheMetadataKey(identity, mediaType),
					"index": "cache/objects/" + identity + "/representation-index.json", "complete": cacheCompleteKey(identity, mediaType)}
				faults := &cacheCommitFailureStore{Store: store, key: keys[stage], dataKey: dataKey, commit: committed}
				cache := NewCache(faults)
				if err := cache.Put(ctx, "ghcr.io", "team/image", digest, mediaType, bytes.NewReader(body), int64(len(body))); !errors.Is(err, storage.ErrUnavailable) || !faults.triggered {
					t.Fatalf("interrupted %s commit = %v, triggered=%t", stage, err, faults.triggered)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = filesystem.New(root)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				faults.Store = store
				cache = NewCache(faults)
				object, readErr := cache.GetAny(ctx, "ghcr.io", "team/image", digest)
				if stage == "complete" && committed {
					if readErr != nil {
						t.Fatalf("committed object after restart: %v", readErr)
					}
					_ = object.Body.Close()
				} else if readErr == nil {
					_ = object.Body.Close()
					t.Fatal("incomplete commit became readable after restart")
				} else if !errors.Is(readErr, storage.ErrNotFound) && !errors.Is(readErr, storage.ErrUnavailable) {
					t.Fatalf("incomplete commit error = %v", readErr)
				}
				if err := cache.Put(ctx, "ghcr.io", "team/image", digest, mediaType, bytes.NewReader(body), int64(len(body))); err != nil {
					t.Fatalf("resume interrupted commit: %v", err)
				}
				object, err = cache.GetAny(ctx, "ghcr.io", "team/image", digest)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := io.ReadAll(object.Body)
				_ = object.Body.Close()
				if err != nil || !bytes.Equal(actual, body) || object.DataVersion == "" || faults.payloadCommits != 1 {
					t.Fatalf("recovered body=%q error=%v version=%q payload commits=%d", actual, err, object.DataVersion, faults.payloadCommits)
				}
			})
		}
	}
}

type cacheCommitFailureStore struct {
	storage.Store
	key, dataKey   string
	commit         bool
	triggered      bool
	payloadCommits int
}

func (s *cacheCommitFailureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	fail := key == s.key && !s.triggered
	if fail {
		s.triggered = true
		if !s.commit {
			return storage.Entry{}, storage.ErrUnavailable
		}
	}
	entry, err := s.Store.Put(ctx, key, body, size, options)
	if key == s.dataKey && err == nil {
		s.payloadCommits++
	}
	if fail && err == nil {
		return storage.Entry{}, storage.ErrUnavailable
	}
	return entry, err
}
