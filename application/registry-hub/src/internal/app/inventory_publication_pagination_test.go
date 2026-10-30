package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInventoryPublicationPaginationPreservesNewerNotificationAndRetiresMissingRepository(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			ctx := context.Background()
			var pages atomic.Int64
			f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/tags/list") {
					pages.Add(1)
					if missing {
						http.NotFound(w, r)
						return
					}
					tags := []string{}
					if r.URL.Query().Get("last") == "" {
						for index := range 100 {
							tags = append(tags, fmt.Sprintf("tag-%03d", index))
						}
					} else if r.URL.Query().Get("last") != "tag-099" {
						t.Errorf("tag pagination lost its last reference: %s", r.URL.RawQuery)
					}
					_ = json.NewEncoder(w).Encode(map[string][]string{"tags": tags})
					return
				}
				if !strings.Contains(r.URL.Path, "/manifests/") {
					t.Errorf("repository notification scanned unrelated source: %s", r.URL.Path)
				}
				http.NotFound(w, r)
			}))
			f.service.inventoryDistributionChanged(ctx, "local-image")
			key := inventoryChangeKey("distribution/repositories/local-image")
			change, _, err := inventory.Read[inventoryChange](ctx, f.service.store, key)
			if err != nil {
				t.Fatal(err)
			}
			p := inventoryDeltaProgress{Changes: []inventoryChange{change}, Phase: 1, Cursor: inventory.Cursor{Done: true}}
			newer := ""
			for step := 0; ; step++ {
				if step == 300 {
					t.Fatal("repository notification did not converge")
				}
				done, err := f.service.advanceInventoryDelta(ctx, &p)
				if err != nil {
					t.Fatal(err)
				}
				if !missing && pages.Load() > 0 && newer == "" {
					f.service.inventoryDistributionChanged(ctx, "local-image")
					value, _, err := inventory.Read[inventoryChange](ctx, f.service.store, key)
					if err != nil {
						t.Fatal(err)
					}
					newer = value.Revision
				}
				data, _ := json.Marshal(p)
				p = inventoryDeltaProgress{}
				if err := json.Unmarshal(data, &p); err != nil {
					t.Fatal(err)
				}
				if done {
					break
				}
			}
			value, _, err := inventory.Read[inventoryChange](ctx, f.service.store, key)
			if missing {
				if !errors.Is(err, storage.ErrNotFound) || pages.Load() != 1 {
					t.Fatalf("removed repository blocked acknowledgement: pages=%d error=%v", pages.Load(), err)
				}
			} else if err != nil || value.Revision != newer || pages.Load() != 2 {
				t.Fatalf("pagination lost newer notification: pages=%d revision=%s expected=%s error=%v", pages.Load(), value.Revision, newer, err)
			}
		})
	}
}
