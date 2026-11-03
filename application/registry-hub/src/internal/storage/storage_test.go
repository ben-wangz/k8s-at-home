package storage_test

import (
	"context"
	"errors"
	"io"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

type probeFaultStore struct {
	storage.Store
	stage   string
	puts    int
	deletes int
	cause   error
}

func (s *probeFaultStore) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	s.puts++
	if s.stage == "create" && s.puts == 2 {
		return storage.Entry{}, s.cause
	}
	return s.Store.Put(ctx, key, body, size, options)
}

func (s *probeFaultStore) DeleteIfMatch(ctx context.Context, key, version string) error {
	s.deletes++
	if s.stage == "delete" && s.deletes == 1 {
		return s.cause
	}
	return s.Store.DeleteIfMatch(ctx, key, version)
}

func TestProbeDistinguishesConditionalViolationsFromStorageFailures(t *testing.T) {
	for _, stage := range []string{"create", "delete"} {
		for _, cause := range []error{storage.ErrTimeout, storage.ErrUnavailable, storage.ErrPermissionDenied, storage.ErrUnknownCommit, nil} {
			name := "ignored_condition"
			if cause != nil {
				name = cause.Error()
			}
			t.Run(stage+"/"+name, func(t *testing.T) {
				base, err := filesystem.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				err = storage.Probe(context.Background(), &probeFaultStore{Store: base, stage: stage, cause: cause})
				if cause == nil {
					if err == nil || !strings.Contains(err.Error(), "does not enforce conditional "+stage) {
						t.Fatalf("capability violation was lost: %v", err)
					}
				} else if !errors.Is(err, cause) || !strings.Contains(err.Error(), "conditional "+stage) || strings.Contains(err.Error(), "does not enforce") {
					t.Fatalf("storage failure was masked: %v", err)
				}
			})
		}
	}
}
