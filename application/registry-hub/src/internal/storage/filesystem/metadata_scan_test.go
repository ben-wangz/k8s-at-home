package filesystem

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"registry-hub/internal/storage"
)

func TestMetadataScanPaginationPreservesGlobalKeyOrder(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expected := []string{"a-b", "a.json", "a/b", "aa"}
	for _, key := range expected {
		if _, err = s.Put(ctx, key, strings.NewReader("data"), 4, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	actual := []string{}
	token := ""
	for {
		items, next, err := s.ListMetadata(ctx, "", token, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			actual = append(actual, item.Key)
			if item.Size != 4 {
				t.Fatal("metadata size is incorrect")
			}
		}
		if next == "" {
			break
		}
		if next == token {
			t.Fatal("cursor did not advance")
		}
		token = next
	}
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("keys=%v expected=%v", actual, expected)
	}
}

func TestMetadataScanAcceptsPartialFilenamePrefixes(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"uploads/session-a", "uploads/session-b", "uploads/other"} {
		if _, err := s.Put(ctx, key, strings.NewReader("data"), 4, storage.PutOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	items, next, err := s.ListMetadata(ctx, "uploads/session", "", 1)
	if err != nil || len(items) != 1 || items[0].Key != "uploads/session-a" || next == "" {
		t.Fatalf("first page=%+v next=%q err=%v", items, next, err)
	}
	items, next, err = s.ListMetadata(ctx, "uploads/session", next, 1)
	if err != nil || len(items) != 1 || items[0].Key != "uploads/session-b" || next != "" {
		t.Fatalf("second page=%+v next=%q err=%v", items, next, err)
	}
}

func TestMetadataFingerprintMatchesHeadWithoutChangingContentVersion(t *testing.T) {
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := "background/v1/receipts/history_cleanup/example.json"
	content := "receipt content"
	if _, err := store.Put(ctx, key, strings.NewReader(content), int64(len(content)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}

	listed, _, err := store.ListMetadata(ctx, key, "", 1)
	if err != nil || len(listed) != 1 {
		t.Fatalf("metadata scan = %+v, error = %v", listed, err)
	}
	head, err := store.Head(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].MetadataFingerprint == "" || listed[0].MetadataFingerprint != head.MetadataFingerprint {
		t.Fatalf("metadata fingerprints differ: listed=%q head=%q", listed[0].MetadataFingerprint, head.MetadataFingerprint)
	}
	if listed[0].Version != "" || head.Version == "" || listed[0].MetadataFingerprint == head.Version {
		t.Fatalf("content and metadata versions were conflated: listed=%+v head=%+v", listed[0], head)
	}
}
