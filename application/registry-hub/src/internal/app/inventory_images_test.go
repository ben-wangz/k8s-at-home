package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type inventoryParallelCaptureStore struct {
	storage.Store
	active atomic.Int64
	peak   atomic.Int64
	joined atomic.Int64
	failed atomic.Bool
	ready  chan struct{}
	fail   string
}

func (s *inventoryParallelCaptureStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	if strings.Contains(key, "/captures/") {
		active := s.active.Add(1)
		defer s.active.Add(-1)
		for peak := s.peak.Load(); active > peak && !s.peak.CompareAndSwap(peak, active); peak = s.peak.Load() {
		}
		if s.joined.Add(1) == 4 {
			close(s.ready)
		}
		select {
		case <-s.ready:
		case <-ctx.Done():
			return storage.Entry{}, ctx.Err()
		}
		if key == s.fail && s.failed.CompareAndSwap(false, true) {
			return storage.Entry{}, storage.ErrTimeout
		}
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func TestInventoryParallelImagesRetainMetadataBarrierAndReplayPartialWrites(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, 1)
	state := inventoryCheckpoint{Build: &writer.Snapshot, ImagesEnrolled: true}
	images := []*inventoryImageProgress{}
	for _, name := range []string{"one", "two", "three", "four"} {
		images = append(images, &inventoryImageProgress{Phase: 6, Image: inventory.Image{ID: inventory.ID(name), Scope: "platform"}})
	}
	state.ImageBuild, state.ParallelImages = images[0], images[1:]
	spy := &inventoryParallelCaptureStore{Store: f.service.store, ready: make(chan struct{}), fail: inventory.Prefix + "snapshots/" + writer.Snapshot.SnapshotID + "/captures/" + images[1].Image.ID + ".json"}
	f.service.store, f.service.inventory.Store = spy, spy
	if err := f.service.advanceInventoryImages(ctx, &state); err != nil {
		t.Fatal(err)
	}
	if spy.joined.Load() != 0 {
		t.Fatal("immutable capture crossed an uncommitted metadata boundary")
	}
	checkpoint, _ := json.Marshal(state)
	if err := f.service.advanceInventoryImages(ctx, &state); !errors.Is(err, storage.ErrTimeout) {
		t.Fatalf("partial capture failure was not returned: %v", err)
	}
	if spy.peak.Load() != 4 || !spy.failed.Load() {
		t.Fatalf("parallel capture fixture did not exercise four independent lanes: peak=%d failed=%t", spy.peak.Load(), spy.failed.Load())
	}
	state = inventoryCheckpoint{}
	if err := json.Unmarshal(checkpoint, &state); err != nil {
		t.Fatal(err)
	}
	for step := 0; state.ImageBuild != nil; step++ {
		if step == 20 {
			t.Fatal("parallel capture replay did not converge")
		}
		if err := f.service.advanceInventoryImages(ctx, &state); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(state)
		state = inventoryCheckpoint{}
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
	}
	for _, image := range images {
		if ready, err := writer.HasImage(ctx, image.Image.ID); err != nil || !ready {
			t.Fatalf("replayed image missing: %s ready=%t error=%v", image.Image.ID, ready, err)
		}
	}
}

func TestInventoryParallelImagesSpillLargeProgressWithoutTruncation(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	enableInventory(f)
	ctx := context.Background()
	writer := f.service.inventory.NewWriter(time.Now().UTC(), time.Hour, 2)
	image := inventoryImageProgress{Image: inventory.Image{ID: inventory.ID("large"), Evidence: inventory.Evidence{Reasons: []string{strings.Repeat("x", 2<<20)}}}}
	other := image
	other.Image.ID = inventory.ID("deferred")
	state := inventoryCheckpoint{Build: &writer.Snapshot, ImagesEnrolled: true, ImageBuild: &image, ParallelImages: []*inventoryImageProgress{&other}}
	if err := f.service.boundInventoryImages(ctx, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.ParallelImages) != 0 || len(state.DeferredImages) != 1 {
		t.Fatalf("large progress was not deferred: inline=%d deferred=%d", len(state.ParallelImages), len(state.DeferredImages))
	}
	data, _ := json.Marshal(state)
	state = inventoryCheckpoint{}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state.ImageBuild = nil
	if err := f.service.buildInventoryPage(ctx, &state); err != nil {
		t.Fatal(err)
	}
	expected, _ := json.Marshal(other)
	actual, _ := json.Marshal(state.ImageBuild)
	if string(actual) != string(expected) || len(state.DeferredImages) != 0 || !state.ImagesEnrolled {
		t.Fatal("deferred progress lost metadata or was not resumed")
	}
}
