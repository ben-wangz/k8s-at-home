package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage/filesystem"
	"registry-hub/internal/tasks"
	"testing"
)

func TestCancelRetainsVerifiedObjectsBeforeRootPublication(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := tasks.NewManager(store, 1, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	cache := artifact.NewCache(store)
	payload := []byte("verified child object retained after cancellation")
	digest := artifact.CalculateSHA256(payload)
	if err := cache.Put(ctx, "ghcr.io", "team/image", digest, ociManifestMediaType, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	task, _, err := manager.Enqueue(ctx, "ghcr.io", "team/image", "latest")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := manager.Cancel(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != tasks.Cancelled || cancelled.RootPublished {
		t.Fatalf("cancelled task state = %#v, want cancelled before root publication", cancelled)
	}
	object, err := cache.Get(ctx, "ghcr.io", "team/image", digest, ociManifestMediaType)
	if err != nil {
		t.Fatalf("verified object was removed when task was cancelled: %v", err)
	}
	actual, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("verified object after cancellation = %q, err=%v", actual, err)
	}
}

func writeManifestMiss(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(writer, `{"errors":[{"code":"MANIFEST_UNKNOWN","message":"manifest unknown"}]}`)
}
