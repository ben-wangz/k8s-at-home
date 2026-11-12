package filesystem

import (
	"bytes"
	"context"
	"errors"
	"hash"
	"io"
	"testing"

	"registry-hub/internal/storage"
)

func TestPutVerifiedRejectsDigestMismatchBeforePublishing(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	body := []byte("payload")
	wrong := []byte("another")
	digest := storage.DigestFromHasher("sha256:00000000000000000000000000000000", digestHash(t, wrong))
	_, err = store.PutVerified(context.Background(), "cache/object", bytes.NewReader(body), int64(len(body)), storage.VerifiedPutOptions{
		PutOptions: storage.PutOptions{IfAbsent: true}, Digest: digest,
	})
	if !errors.Is(err, storage.ErrDigestMismatch) {
		t.Fatalf("PutVerified() = %v, want digest mismatch", err)
	}
	if _, err := store.Head(context.Background(), "cache/object"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("mismatched payload was published: Head() = %v", err)
	}
}

func TestPutVerifiedMismatchPreservesExistingObject(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	existing := []byte("existing")
	entry, err := store.Put(ctx, "cache/object", bytes.NewReader(existing), int64(len(existing)), storage.PutOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte("wrong")
	digest := storage.DigestFromHasher("sha256:00000000000000000000000000000000", digestHash(t, existing))
	_, err = store.PutVerified(ctx, "cache/object", bytes.NewReader(wrong), int64(len(wrong)), storage.VerifiedPutOptions{
		PutOptions: storage.PutOptions{IfMatch: entry.Version}, Digest: digest,
	})
	if !errors.Is(err, storage.ErrDigestMismatch) {
		t.Fatalf("PutVerified() = %v, want digest mismatch", err)
	}
	object, err := store.Get(ctx, "cache/object")
	if err != nil {
		t.Fatal(err)
	}
	actual, readErr := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(actual, existing) {
		t.Fatalf("existing payload=%q read=%v close=%v", actual, readErr, closeErr)
	}
}

func digestHash(t *testing.T, body []byte) hash.Hash {
	t.Helper()
	hasher, err := storage.NewDigestHasher("sha256:00000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = hasher.Write(body)
	return hasher
}
