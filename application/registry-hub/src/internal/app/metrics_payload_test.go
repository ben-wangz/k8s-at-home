package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"registry-hub/internal/artifact"
	"registry-hub/internal/storage"
	"registry-hub/internal/storage/filesystem"
	"testing"
)

func TestObservedStorePreservesPayloadCapabilityAndCountsHashing(t *testing.T) {
	base, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := newMetrics()
	store := newObservedStore(&recoveryStore{Store: base}, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cache := artifact.NewCache(store)
	data := bytes.Repeat([]byte("observed layer"), 1<<15)
	digest := artifact.CalculateSHA256(data)
	ctx := context.Background()
	if err := cache.Put(ctx, "example.test", "team/image", digest, "application/octet-stream", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.VerifyAny(ctx, "example.test", "team/image", digest); err != nil {
			t.Fatal(err)
		}
	}
	for _, operation := range []string{"read", "hash"} {
		got := payloadMetric(t, m, "cache_verification", operation)
		if got != float64(2*len(data)) {
			t.Fatalf("wrapped verification %s=%v, want %d", operation, got, 2*len(data))
		}
	}
	object, err := cache.GetAny(ctx, "example.test", "team/image", digest)
	if err != nil {
		t.Fatal(err)
	}
	count, err := io.Copy(io.Discard, object.Body)
	closeErr := object.Body.Close()
	if err != nil || closeErr != nil || count != int64(len(data)) {
		t.Fatalf("wrapped consumption bytes=%d error=%v close=%v", count, err, closeErr)
	}
	if got := payloadMetric(t, m, "cache_verification", "hash"); got != float64(2*len(data)) {
		t.Fatalf("normal read repeated cache hashes: %v", got)
	}
	if _, err := store.Put(ctx, "probe", bytes.NewReader(data), int64(len(data)), storage.PutOptions{}); err != nil {
		t.Fatal(err)
	}
	probeCtx := storage.WithIOPurpose(ctx, "cleanup_verification")
	entry, err := store.Head(probeCtx, "probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteIfMatch(probeCtx, "probe", entry.Version); err != nil {
		t.Fatal(err)
	}
	if got := payloadMetric(t, m, "cleanup_verification", "hash"); got != float64(2*len(data)) {
		t.Fatalf("filesystem Head/DeleteIfMatch hashing was hidden: %v", got)
	}
}

func payloadMetric(t *testing.T, m *metrics, purpose, operation string) float64 {
	t.Helper()
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "registry_hub_payload_bytes_total" {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string)
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["kind"] == "service" && labels["purpose"] == purpose && labels["operation"] == operation {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
