package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"registry-hub/internal/artifact"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"testing"
)

func TestInventoryAuditResumesHashAndRejectsChangedEpoch(t *testing.T) {
	for _, changeEpoch := range []bool{false, true} {
		t.Run(fmt.Sprint(changeEpoch), func(t *testing.T) {
			f := newStagedAcceptanceFixture(t, 1<<20)
			enableInventory(f)
			data := bytes.Repeat([]byte("chunk-audit"), 500000)
			digest := artifact.CalculateSHA256(data)
			ranges := []string{}
			f.service.registry = newLocalRegistry(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ranges = append(ranges, r.Header.Get("Range"))
				w.Header().Set("Docker-Content-Digest", digest)
				http.ServeContent(w, r, "", f.service.inventoryInstalledAt, bytes.NewReader(data))
			}))
			identity := inventoryIdentity("", "local/image", "")
			object := inventory.Object{ID: identity.ContentID(digest), Identity: identity, ObjectRef: inventory.ObjectRef{Role: "layer", Digest: digest, Size: int64(len(data))}}
			ctx := context.Background()
			p := &inventoryAuditProgress{Object: object, Path: "/v2/local/image/blobs/" + digest, Epoch: f.service.gate.State().Epoch}
			done, err := f.service.advanceInventoryAudit(ctx, p)
			if err != nil || done || p.Offset != 4<<20 {
				t.Fatalf("first chunk: done=%v offset=%d err=%v", done, p.Offset, err)
			}
			encoded, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			p = &inventoryAuditProgress{}
			if err := json.Unmarshal(encoded, p); err != nil {
				t.Fatal(err)
			}
			if changeEpoch {
				if err := inventory.Update(ctx, f.service.store, "system/gc/state.json", func(value *gateRecord) error { value.Epoch++; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			done, err = f.service.advanceInventoryAudit(ctx, p)
			if changeEpoch {
				if done || !errors.Is(err, storage.ErrConditionFailed) {
					t.Fatalf("changed epoch accepted: done=%v err=%v", done, err)
				}
				if _, _, err := inventory.Read[inventoryProof](ctx, f.service.store, inventory.Prefix+"proofs/"+object.ID+".json"); !errors.Is(err, storage.ErrNotFound) {
					t.Fatalf("invalid audit wrote proof: %v", err)
				}
				return
			}
			if err != nil || !done || len(ranges) != 2 || ranges[1] == ranges[0] {
				t.Fatalf("resumed audit: done=%v ranges=%v err=%v", done, ranges, err)
			}
		})
	}
}

func TestInventoryStatusAvailableBeforeSnapshot(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	guard := &inventoryWriteGuard{Store: f.service.store}
	f.service.store, f.service.inventory.Store = guard, guard
	request := httptest.NewRequest(http.MethodGet, "/api/v1/images/status", nil)
	response := httptest.NewRecorder()
	f.service.serveInventory(response, request, context.Background())
	if response.Code != http.StatusOK || guard.writes != 0 {
		t.Fatalf("status=%d writes=%d body=%s", response.Code, guard.writes, response.Body.String())
	}
}
