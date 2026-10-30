package gc

import (
	"context"
	"fmt"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type gcRestartEvidence struct {
	Unknown, Recovered, Job map[string]any
}

func verifyGCRestartRecovery(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient, repository string, retained support.ImageFixture) (gcRestartEvidence, error) {
	var evidence gcRestartEvidence
	started, err := startGC(ctx, environment)
	if err != nil {
		return evidence, err
	}
	job, err := waitForGCJob(ctx, environment, testkit.String(started["id"]))
	if err != nil {
		return evidence, err
	}
	jobName := testkit.String(job["jobName"])
	if err := setGCJobSuspended(ctx, environment, jobName, true); err != nil {
		return evidence, fmt.Errorf("pause the GC Job before restarting backend: %w", err)
	}
	if err := restartBackendPod(ctx, environment); err != nil {
		return evidence, err
	}
	if err := useBackendPodForGC(ctx, environment, registry); err != nil {
		return evidence, err
	}
	evidence.Unknown, err = waitForGCUnknown(ctx, environment)
	if err != nil {
		return evidence, err
	}
	if err := verifyGCWriteAdmission(ctx, registry, repository); err != nil {
		return evidence, fmt.Errorf("unknown GC state did not keep writes closed: %w", err)
	}
	if err := verifyGCReadAdmission(ctx, registry, repository, retained); err != nil {
		return evidence, fmt.Errorf("Registry reads failed during unknown GC state: %w", err)
	}
	if err := verifyGCRecoveryRemainsLocked(ctx, environment, testkit.String(evidence.Unknown["id"])); err != nil {
		return evidence, err
	}
	if err := setGCJobSuspended(ctx, environment, jobName, false); err != nil {
		return evidence, fmt.Errorf("resume the GC Job after backend restart: %w", err)
	}
	if err := waitForGCJobSuccess(ctx, environment, jobName); err != nil {
		return evidence, err
	}
	evidence.Recovered, err = recoverGC(ctx, environment, testkit.String(evidence.Unknown["id"]))
	if err != nil {
		return evidence, err
	}
	evidence.Job, err = verifyGCJob(ctx, environment, started, evidence.Recovered)
	if err != nil {
		return evidence, err
	}
	if err := verifyGCReadAdmission(ctx, registry, repository, retained); err != nil {
		return evidence, fmt.Errorf("Registry reads failed after GC recovery: %w", err)
	}
	return evidence, nil
}
