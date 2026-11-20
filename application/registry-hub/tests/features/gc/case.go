package gc

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreammirror "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/upstream-mirror"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"io"
	"net/http"
	"strings"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-gc")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	registryConfig, err := registryConfigValue(ctx, environment)
	if err != nil {
		return err
	}
	if strings.Contains(registryConfig, "blobdescriptor: inmemory") || !strings.Contains(registryConfig, "uploadpurging:\n      enabled: false") {
		return fmt.Errorf("GC requires a disabled Registry descriptor cache and upload purging")
	}
	upstreamRegistry, upstreamRepository, err := upstreammirror.PrepareExternalFixture(ctx, environment)
	if err != nil {
		return fmt.Errorf("prepare controlled upstream fixture: %w", err)
	}
	registry := support.NewRegistryClient(environment)
	clientAuth, err := clientAuthWriter(ctx, environment)
	if err != nil {
		return err
	}
	registry.Basic = &clientAuth
	upstreamRegistry.Basic = &clientAuth
	if err := useBackendPodForGC(ctx, environment, registry); err != nil {
		return err
	}
	repository := support.LocalRepository(environment.CaseID, environment.RunID)
	retained, err := support.BuildImageFixture(ctx, registry, repository, "retained", environment.RunID)
	if err != nil {
		return err
	}
	orphanBytes := []byte("registry-hub-gc-orphan:" + environment.RunID)
	orphanDigest, err := registry.PutBlob(ctx, repository, orphanBytes)
	if err != nil {
		return fmt.Errorf("upload unreferenced blob: %w", err)
	}
	actualOrphan, err := support.ReadBlob(ctx, registry, repository, orphanDigest)
	if err != nil {
		return fmt.Errorf("unreferenced blob was not readable before GC: %w", err)
	}
	if !bytes.Equal(actualOrphan, orphanBytes) {
		return fmt.Errorf("unreferenced blob bytes changed before GC")
	}
	if err := verifyPVCConsumers(ctx, environment, ""); err != nil {
		return err
	}
	if err := verifyRegistryIngressPolicy(ctx, environment); err != nil {
		return err
	}
	activeWriter, err := startActiveUploadWriter(ctx, registry, repository, environment.RunID)
	if err != nil {
		return err
	}
	defer activeWriter.writer.Close()
	if err := waitForActiveUploadAdmission(ctx, environment, activeWriter.result); err != nil {
		return err
	}
	configRestoreRequired := true
	defer func() {
		if configRestoreRequired {
			_ = patchRegistryConfig(context.Background(), environment, registryConfig)
		}
	}()
	if err := patchRegistryConfig(ctx, environment, "invalid: ["); err != nil {
		return fmt.Errorf("inject a malformed GC Job config: %w", err)
	}

	started, err := startGC(ctx, environment)
	if err != nil {
		return err
	}
	if err := verifyGCWaitsForActiveWriter(ctx, environment, activeWriter.result); err != nil {
		return err
	}
	if err := verifyGCWriteAdmission(ctx, registry, repository, "5"); err != nil {
		return err
	}
	if err := verifyGCReadAdmission(ctx, registry, repository, retained); err != nil {
		return err
	}
	activeWriterStatus, err := finishActiveUploadWriter(activeWriter)
	if err != nil {
		return err
	}
	failed, err := waitForGCFailure(ctx, environment)
	if err != nil {
		return err
	}
	failedJobDetails, err := verifyFailedGCJob(ctx, environment, started, failed)
	if err != nil {
		return err
	}
	if err := verifyGCWriteAdmission(ctx, registry, repository, "5"); err != nil {
		return fmt.Errorf("failed GC did not keep writes closed: %w", err)
	}
	if err := verifyGCReadAdmission(ctx, registry, repository, retained); err != nil {
		return fmt.Errorf("Registry reads failed while GC was failed: %w", err)
	}
	if err := patchRegistryConfig(ctx, environment, registryConfig); err != nil {
		return fmt.Errorf("restore the GC Job config after the injected failure: %w", err)
	}
	configRestoreRequired = false
	retried, err := retryGC(ctx, environment)
	if err != nil {
		return err
	}
	if err := cleanupGCJob(ctx, environment, testkit.String(failedJobDetails["name"]), testkit.String(failedJobDetails["uid"])); err != nil {
		return err
	}
	finished, err := waitForGC(ctx, environment)
	if err != nil {
		return err
	}
	jobDetails, err := verifyGCJob(ctx, environment, retried, finished)
	if err != nil {
		return err
	}
	if err := verifyPVCConsumers(ctx, environment, testkit.String(jobDetails["name"])); err != nil {
		return err
	}
	if err := verifyGCContents(ctx, registry, repository, retained, orphanDigest); err != nil {
		return err
	}
	reuploadedDigest, err := registry.PutBlob(ctx, repository, orphanBytes)
	if err != nil {
		return fmt.Errorf("re-upload blob collected by GC: %w", err)
	}
	if reuploadedDigest != orphanDigest {
		return fmt.Errorf("re-uploaded blob digest = %s, want %s", reuploadedDigest, orphanDigest)
	}
	reuploadedBytes, err := support.ReadBlob(ctx, registry, repository, reuploadedDigest)
	if err != nil {
		return fmt.Errorf("read re-uploaded blob after GC: %w", err)
	}
	if !bytes.Equal(reuploadedBytes, orphanBytes) {
		return fmt.Errorf("re-uploaded blob bytes changed after GC")
	}
	if err := verifyGCMetrics(ctx, environment, 1, 1, 2); err != nil {
		return err
	}
	restart, err := verifyGCRestartRecovery(ctx, environment, registry, repository, retained)
	if err != nil {
		return err
	}
	if err := cleanupGCJob(ctx, environment, testkit.String(jobDetails["name"]), testkit.String(jobDetails["uid"])); err != nil {
		return err
	}
	if err := cleanupGCJob(ctx, environment, testkit.String(restart.Job["name"]), testkit.String(restart.Job["uid"])); err != nil {
		return err
	}
	if err := verifyPVCConsumers(ctx, environment, ""); err != nil {
		return err
	}
	if err := waitForRegistryService(ctx, upstreamRegistry); err != nil {
		return err
	}
	solidificationGC, err := verifySolidificationCancellationGC(ctx, environment, upstreamRegistry, upstreamRepository)
	if err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"gcFailureID": testkit.String(failed["id"]), "gcFailureState": testkit.String(failed["state"]),
		"gcFailureClass": testkit.String(failed["errorClass"]), "failedJobName": failedJobDetails["name"],
		"failedJobUID": failedJobDetails["uid"], "failedJobPodName": failedJobDetails["podName"],
		"failedJobPodCount": failedJobDetails["podCount"], "failedJobCleaned": true,
		"gcRetryID": testkit.String(retried["id"]), "gcState": testkit.String(finished["state"]),
		"gcUnknownID": testkit.String(restart.Unknown["id"]), "gcUnknownState": testkit.String(restart.Unknown["state"]),
		"gcUnknownClass": testkit.String(restart.Unknown["errorClass"]), "gcRecoveryID": testkit.String(restart.Recovered["id"]),
		"gcRecoveryState": testkit.String(restart.Recovered["state"]), "gcForcedRecoveryRejected": true, "unknownJobCleaned": true,
		"writeDuringUnknownStatus": http.StatusServiceUnavailable, "readDuringUnknownStatus": http.StatusOK,
		"recoveryJobName": restart.Job["name"], "recoveryJobUID": restart.Job["uid"],
		"gateState":    testkit.String(finished["_gateState"]),
		"configDigest": testkit.String(finished["configDigest"]),
		"jobName":      jobDetails["name"], "jobUID": jobDetails["uid"], "jobImage": jobDetails["image"],
		"jobConfigDigest": jobDetails["configDigest"], "registryPVC": jobDetails["registryPVC"],
		"retainedManifestDigest": retained.ManifestDigest, "retainedLayerDigest": firstLayerDigest(retained),
		"orphanBlobDigest": orphanDigest, "orphanBlobStatusAfterGC": http.StatusNotFound,
		"reuploadedBlobDigest": reuploadedDigest, "gcDescriptorCacheDisabled": true, "gcJobCleaned": true,
		"gcSuccessMetric": 1, "gcFailureMetric": 1, "gcDurationSamples": 2, "writeDuringGCStatus": http.StatusServiceUnavailable,
		"activeUploadWriteStatus": activeWriterStatus, "gcWaitedForActiveWriter": true,
		"registryPVCConsumersVerified": true, "registryIngressPolicyVerified": true,
		"readDuringGCStatus":     http.StatusOK,
		"solidificationCancelGC": solidificationGC,
	})
}

func clientAuthWriter(ctx context.Context, environment testkit.CaseContext) (testkit.BasicCredential, error) {
	secret, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+"-external-client-auth")
	if err != nil {
		return testkit.BasicCredential{}, err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" {
		return testkit.BasicCredential{}, fmt.Errorf("GC client auth Secret ownership is invalid")
	}
	var auth support.AdminAuth
	if err := json.Unmarshal(data["config.json"], &auth); err != nil {
		return testkit.BasicCredential{}, err
	}
	if len(auth.Basic) != 3 || auth.Basic[2].Username != "writer" || len(auth.Basic[2].Password) < 32 ||
		len(auth.Basic[2].Permissions) != 3 || auth.Basic[2].Permissions[0] != "pull" ||
		auth.Basic[2].Permissions[1] != "push" || auth.Basic[2].Permissions[2] != "delete" {
		return testkit.BasicCredential{}, fmt.Errorf("GC client auth writer does not have pull, push, and delete permissions")
	}
	return testkit.BasicCredential{Username: auth.Basic[2].Username, Password: auth.Basic[2].Password}, nil
}

type activeUploadWriter struct {
	writer *io.PipeWriter
	result chan activeUploadResult
	last   []byte
}

type activeUploadResult struct {
	status int
	err    error
}
