package app

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/s3store"
	"testing"
	"time"
)

func newDedicatedS3AcceptanceStore(t *testing.T, variable string) storage.Store {
	t.Helper()
	raw := os.Getenv(variable)
	if raw == "" {
		t.Skipf("set %s to a dedicated test bucket configuration", variable)
	}
	var cfg config.S3
	if json.Unmarshal([]byte(raw), &cfg) != nil || cfg.Bucket == "" {
		t.Fatal("invalid dedicated S3 acceptance configuration")
	}
	cfg.Prefix = path.Join(cfg.Prefix, "acceptance", requestID())
	cfg.ReplayDirectory = t.TempDir()
	if cfg.MultipartThreshold == 0 {
		cfg.MultipartThreshold = 64 << 20
	}
	if cfg.PartSize == 0 {
		cfg.PartSize = 16 << 20
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 16
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	remote, err := s3store.New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer remote.Close()
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for {
			entries, _, err := remote.List(ctx, "", "", 100)
			if err != nil {
				t.Errorf("list dedicated S3 acceptance prefix for cleanup: %v", err)
				return
			}
			if len(entries) == 0 {
				return
			}
			if !deleteS3AcceptanceEntries(t, ctx, remote, entries) {
				return
			}
		}
	})
	return remote
}

func deleteS3AcceptanceEntries(t *testing.T, ctx context.Context, store storage.Store, entries []storage.Entry) bool {
	t.Helper()
	for offset := 0; offset < len(entries); offset += 4 {
		batch := entries[offset:min(offset+4, len(entries))]
		results := make(chan error, len(batch))
		for _, entry := range batch {
			go func() { results <- store.Delete(ctx, entry.Key) }()
		}
		failed := false
		for range batch {
			if err := <-results; err != nil {
				t.Errorf("clean dedicated S3 acceptance prefix: %v", err)
				failed = true
			}
		}
		if failed {
			return false
		}
	}
	return true
}
