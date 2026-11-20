package observability

import (
	"context"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func exerciseInventorySnapshotMetrics(ctx context.Context, environment testkit.CaseContext) (map[string]any, error) {
	repository := support.LocalRepository("observability-metrics", environment.RunID)
	fixture, err := support.BuildImageFixture(ctx, support.NewRegistryClient(environment), repository, "metrics", environment.RunID)
	if err != nil {
		return nil, fmt.Errorf("create inventory metrics fixture: %w", err)
	}
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	for {
		metrics, err := readMetrics(ctx, environment)
		if err != nil {
			return nil, err
		}
		writes, writesFound, err := testkit.PrometheusSum(metrics, "registry_hub_inventory_snapshot_object_writes_total", nil)
		if err != nil {
			return nil, err
		}
		bytesWritten, bytesFound, err := testkit.PrometheusSum(metrics, "registry_hub_inventory_snapshot_object_write_bytes_total", nil)
		if err != nil {
			return nil, err
		}
		if writesFound && writes > 0 && bytesFound && bytesWritten > 0 {
			return map[string]any{"repository": repository, "manifestDigest": fixture.ManifestDigest, "snapshotWrites": writes, "snapshotWriteBytes": bytesWritten}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("inventory snapshot publication metrics did not advance for %s", repository)
		case <-time.After(time.Second):
		}
	}
}
