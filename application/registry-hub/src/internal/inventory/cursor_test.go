package inventory

import (
	"context"
	"encoding/json"
	"registry-hub/internal/storage"
	"testing"
)

type opaqueCursorStore struct {
	storage.Store
	tokens []string
}

func (s *opaqueCursorStore) List(_ context.Context, _, token string, _ int) ([]storage.Entry, string, error) {
	s.tokens = append(s.tokens, token)
	switch token {
	case "":
		return []storage.Entry{{Key: "one"}, {Key: "two"}}, "opaque-next", nil
	case "opaque-next":
		return nil, "opaque-empty", nil
	case "opaque-empty":
		return []storage.Entry{{Key: "three"}}, "", nil
	default:
		return nil, "", storage.ErrCorruptObject
	}
}

func TestCursorResumesWithinOpaquePagesAndEmptyContinuations(t *testing.T) {
	store := &opaqueCursorStore{}
	cursor := Cursor{}
	seen := []string{}
	for n := 0; !cursor.Done && n < 10; n++ {
		entry, ok, err := cursor.Peek(context.Background(), store, "prefix/", 100)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			seen = append(seen, entry.Key)
			cursor.Advance()
		}
		data, err := json.Marshal(cursor)
		if err != nil {
			t.Fatal(err)
		}
		cursor = Cursor{}
		if err := json.Unmarshal(data, &cursor); err != nil {
			t.Fatal(err)
		}
	}
	if !cursor.Done || len(seen) != 3 || seen[0] != "one" || seen[1] != "two" || seen[2] != "three" {
		t.Fatalf("items=%v done=%v", seen, cursor.Done)
	}
	if len(store.tokens) != 3 || store.tokens[1] != "opaque-next" || store.tokens[2] != "opaque-empty" {
		t.Fatalf("page replay or derived token: %v", store.tokens)
	}
}
