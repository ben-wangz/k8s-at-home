package quotaconvergence

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreammirror "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/upstream-mirror"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "quota-convergence")
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, err
}

func Run(ctx context.Context, environment testkit.CaseContext) (runErr error) {
	registry, repository, err := upstreammirror.PrepareExternalFixture(ctx, environment)
	if err != nil {
		return err
	}
	if err := seedHistoricalTasks(ctx, environment); err != nil {
		return err
	}
	defer func() {
		restore, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Minute)
		defer cancel()
		runErr = errors.Join(runErr, upstreammirror.SetBackendEnvironment(restore, environment, "REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTION_SESSIONS-", "REGISTRY_HUB_SOLIDIFICATION_MAX_UNANCHORED_DISTRIBUTION_BYTES-", "REGISTRY_HUB_WORKERS=4"))
	}()
	if err := upstreammirror.SetBackendEnvironment(ctx, environment, "REGISTRY_HUB_SOLIDIFICATION_MAX_PROTECTION_SESSIONS=1", "REGISTRY_HUB_SOLIDIFICATION_MAX_UNANCHORED_DISTRIBUTION_BYTES=1024", "REGISTRY_HUB_WORKERS=1"); err != nil {
		return err
	}
	config := []byte(`{"architecture":"amd64","os":"linux","quotaFixture":true}`)
	layer := []byte("registry-hub quota convergence layer\n")
	configID, layerID := support.Digest(config), support.Digest(layer)
	blockerID := support.Digest([]byte("{}"))
	if err := cacheBlob(ctx, registry, repository, blockerID, []byte("{}")); err != nil {
		return err
	}
	blocker, err := upstreammirror.WaitForTaskStatus(ctx, environment, blockerID, "verified_local", 3*time.Minute)
	if err != nil {
		return err
	}
	ref := "quota-convergence-" + environment.RunID
	if _, _, err := support.ReadManifest(ctx, registry, repository, ref, support.OCIManifestMediaType); err != nil {
		return err
	}
	if _, err := upstreammirror.WaitForTaskStatus(ctx, environment, ref, "waiting_capacity", 3*time.Minute); err != nil {
		return err
	}
	if err := upstreammirror.SetBackendEnvironment(ctx, environment, "REGISTRY_HUB_WORKERS=4"); err != nil {
		return err
	}
	if err := cacheBlob(ctx, registry, repository, configID, config); err != nil {
		return err
	}
	if err := cacheBlob(ctx, registry, repository, layerID, layer); err != nil {
		return err
	}
	waiting, err := upstreammirror.WaitForTaskStatus(ctx, environment, layerID, "waiting_capacity", 3*time.Minute)
	if err != nil || testkit.String(waiting["waitReason"]) != "distribution_quota" {
		return fmt.Errorf("layer is not waiting on Distribution quota: %v: %w", waiting, err)
	}
	count, err := countUnrelated(ctx, environment)
	if err != nil || count < 100 {
		return fmt.Errorf("unrelated task evidence: count=%d: %w", count, err)
	}
	if err := cancelBlocker(ctx, environment, testkit.String(blocker["id"])); err != nil {
		return err
	}
	root, err := upstreammirror.WaitForTaskStatus(ctx, environment, ref, "solidified", 5*time.Minute)
	if err != nil || root["rootPublished"] != true {
		return fmt.Errorf("platform did not anchor: %v: %w", root, err)
	}
	finished, err := upstreammirror.WaitForTaskStatus(ctx, environment, layerID, "solidified", time.Minute)
	if err != nil {
		return err
	}
	timing, err := convergenceTiming(finished, root)
	if err != nil {
		return err
	}
	for digest, expected := range map[string]int{configID: 0, layerID: 1} {
		requests, err := upstreammirror.CountFixturePath(ctx, environment, "/v2/registry-hub-test/fixture/blobs/"+digest)
		if err != nil || requests != expected {
			return fmt.Errorf("cached blob fetched %d times: %s: %w", requests, digest, err)
		}
	}
	return support.WriteResult(environment, map[string]any{"timing": timing, "unrelatedTasks": count, "unrelatedTaskSource": "historical_completed_records", "ordinarySlots": 2, "measurementTaskWorkers": 4, "setupTaskWorkers": 1, "rootTask": root["id"], "layerTask": finished["id"], "upstreamLayerRequests": 1, "configSource": "inline_verified_cache"})
}
