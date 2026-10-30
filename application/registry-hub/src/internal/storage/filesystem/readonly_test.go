package filesystem

import (
	"context"
	"errors"
	"strings"
	"testing"

	"registry-hub/internal/storage"
)

func TestInspectionStoreRejectsEveryWrite(t *testing.T) {
	s, err := OpenReadOnly(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err = s.Put(ctx, "created", strings.NewReader("x"), 1, storage.PutOptions{}); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("put=%v", err)
	}
	if err = s.Delete(ctx, "missing"); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("delete=%v", err)
	}
	if err = s.DeleteIfMatch(ctx, "missing", "v"); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("conditional delete=%v", err)
	}
}
