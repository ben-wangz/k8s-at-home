package app

import (
	"context"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"registry-hub/internal/tasks"
	"strings"
	"testing"
)

type blobEvidenceFailure struct {
	storage.Store
	prefix string
}

func (s blobEvidenceFailure) Get(ctx context.Context, key string) (storage.Object, error) {
	if strings.HasPrefix(key, s.prefix) {
		return storage.Object{}, storage.ErrUnavailable
	}
	return s.Store.Get(ctx, key)
}

func TestBlobDependencyDistinguishesCleanupAbsenceAndUncertainEvidence(t *testing.T) {
	for _, mode := range []string{"cleanup_pending", "deleted", "corrupt_marker", "corrupt_index", "version_changed", "verification_missing", "read_failure", "intent_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			ctx := context.Background()
			object := f.cacheObject([]byte("verified representation"), "application/octet-stream")
			root := tasks.Task{ID: "evidence", Authority: "ghcr.io", Repository: "team/image"}
			edge := tasks.DependencyEdge{ChildRole: tasks.GraphBlob, ChildDigest: object.Digest, Descriptor: tasks.GraphEntry{Size: object.Size, MediaType: testOCIConfigMediaType}}
			prefix := "cache/objects/" + cacheStorageIdentity(root.Authority, root.Repository, object.Digest) + "/"
			entries, _, err := storage.Scan(ctx, f.service.store, prefix, "", 32)
			if err != nil {
				t.Fatal(err)
			}
			want := "unknown"
			switch mode {
			case "cleanup_pending", "verification_missing":
				for _, entry := range entries {
					if !strings.Contains(entry.Key, "/representations/") {
						continue
					}
					if err := inventory.Update(ctx, f.service.store, entry.Key, func(value *map[string]any) error {
						if mode == "cleanup_pending" {
							(*value)["state"] = "cleanup_pending"
						} else {
							(*value)["verifiedAt"] = nil
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cleanup_pending" {
					want = "complete"
				}
			case "deleted":
				if err := f.service.store.Delete(ctx, prefix+"data"); err != nil {
					t.Fatal(err)
				}
				want = "missing"
			case "version_changed":
				if _, err := f.service.store.Put(ctx, prefix+"data", strings.NewReader("replacement"), 11, storage.PutOptions{}); err != nil {
					t.Fatal(err)
				}
			case "corrupt_marker", "corrupt_index":
				for _, entry := range entries {
					match := strings.Contains(entry.Key, "/complete/")
					if mode == "corrupt_index" {
						match = strings.HasSuffix(entry.Key, "/representation-index.json")
					}
					if match {
						if _, err := f.service.store.Put(ctx, entry.Key, strings.NewReader("invalid"), 7, storage.PutOptions{}); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "read_failure":
				f.service.cache = artifact.NewCache(blobEvidenceFailure{Store: f.service.store, prefix: prefix})
			case "intent_failure":
				if err := f.service.store.Delete(ctx, prefix+"data"); err != nil {
					t.Fatal(err)
				}
				f.service.records = tasks.NewRecords(blobEvidenceFailure{Store: f.service.store, prefix: "solidification/blobs/"})
			}
			view := f.service.taskDependency(ctx, root, edge)
			if view.CacheState != want {
				t.Fatalf("mode=%s view=%+v want=%s", mode, view, want)
			}
		})
	}
}
