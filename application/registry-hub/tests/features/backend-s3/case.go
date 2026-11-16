package backends3

import (
	"context"
	"embed"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"path/filepath"
	"strings"
)

//go:embed case.json
var metadata embed.FS

const backendS3SecretSuffix = "-external-backend-s3"
const backendS3Prefix = "registry-hub-backend"
const backendGCStateKey = backendS3Prefix + "/system/gc/state.json"

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-backend-s3")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secretName := environment.Release + backendS3SecretSuffix
	secret, data, err := support.GetSecret(ctx, environment.Namespace, secretName)
	if err != nil {
		return err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" ||
		len(data) != 2 || len(data["AWS_ACCESS_KEY_ID"]) < 32 || len(data["AWS_SECRET_ACCESS_KEY"]) < 32 {
		return fmt.Errorf("backend S3 credentials Secret ownership or shape is invalid")
	}
	if err := verifyBackendS3Environment(ctx, environment, secretName); err != nil {
		return err
	}
	fingerprint := support.SecretHashes(data)
	endpoint, err := s3ServiceEndpoint(ctx, environment, secretName)
	if err != nil {
		return err
	}
	bucket := "rh-" + environment.RunID
	client, err := newS3Client(ctx, endpoint, string(data["AWS_ACCESS_KEY_ID"]), string(data["AWS_SECRET_ACCESS_KEY"]))
	if err != nil {
		return err
	}
	if err := rejectWrongCredentials(ctx, endpoint, bucket); err != nil {
		return err
	}
	beforeObjects, err := listBackendObjects(ctx, client, bucket)
	if err != nil {
		return err
	}
	if len(beforeObjects) == 0 {
		return fmt.Errorf("backend has not persisted any state objects to the configured S3 prefix")
	}

	upstream := environment.DependencyResults["upstream-persistence-fixture"]
	fixtureLogs, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+secretName, "-c", "versitygw")
	if err != nil {
		return err
	}
	if strings.Contains(string(fixtureLogs.Stdout), string(data["AWS_SECRET_ACCESS_KEY"])) {
		return fmt.Errorf("backend S3 fixture access log exposed the secret access key")
	}
	taskID, err := support.DetailString(upstream, "solidifiedTaskID")
	if err != nil {
		return err
	}
	taskStateKey := backendS3Prefix + "/tasks/state/" + taskID + ".json"
	if _, exists := beforeObjects[taskStateKey]; !exists {
		return fmt.Errorf("solidified task state is missing from the S3 object snapshot")
	}
	manifestDigest, err := support.DetailString(upstream, "successfulManifestDigest")
	if err != nil {
		return err
	}
	repository, err := support.DetailString(upstream, "successfulRepository")
	if err != nil {
		return err
	}
	reference, err := support.DetailString(upstream, "successfulReference")
	if err != nil {
		return err
	}
	taskBefore, err := getTask(ctx, environment, taskID)
	if err != nil {
		return err
	}
	if !solidifiedTask(taskBefore, taskID, manifestDigest) {
		return fmt.Errorf("solidified task state is invalid before restart")
	}
	registry := support.NewRegistryClient(environment)
	manifestBefore, headersBefore, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	if support.Digest(manifestBefore) != manifestDigest || !strings.EqualFold(headersBefore.Get("Docker-Content-Digest"), manifestDigest) {
		return fmt.Errorf("solidified manifest does not match its recorded digest before restart")
	}
	resumeEvidence, err := verifyBackendS3Resume(ctx, environment, client, bucket, registry)
	if err != nil {
		return err
	}
	metrics, err := readS3AttemptMetrics(ctx, environment)
	if err != nil {
		return err
	}
	multipartBlobDigest := testkit.String(resumeEvidence["digest"])
	multipartBlobSize := testkit.Int(resumeEvidence["size"])
	storedMultipartSize := testkit.Int(resumeEvidence["multipartObjectSize"])
	multipartETag := testkit.String(resumeEvidence["multipartETag"])
	multipartParts := testkit.Int(resumeEvidence["multipartParts"])
	beforeObjects, err = listBackendObjects(ctx, client, bucket)
	if err != nil {
		return err
	}
	if err := testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "before-restart-objects.json"), beforeObjects); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "restart", "deployment/"+environment.Fullname); err != nil {
		return err
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=5m"); err != nil {
		return err
	}
	taskAfter, err := getTask(ctx, environment, taskID)
	if err != nil {
		return err
	}
	if !solidifiedTask(taskAfter, taskID, manifestDigest) {
		return fmt.Errorf("solidified task state did not survive backend restart")
	}
	manifestAfter, headersAfter, err := support.ReadManifest(ctx, registry, repository, reference, support.OCIManifestMediaType)
	if err != nil {
		return err
	}
	if string(manifestBefore) != string(manifestAfter) || !strings.EqualFold(headersAfter.Get("Docker-Content-Digest"), manifestDigest) {
		return fmt.Errorf("solidified manifest bytes changed across backend restart")
	}
	afterObjects, err := listBackendObjects(ctx, client, bucket)
	if err != nil {
		return err
	}
	if err := testkit.WriteJSONAtomic(filepath.Join(environment.OutputDir, "after-restart-objects.json"), afterObjects); err != nil {
		return err
	}
	if _, exists := afterObjects[taskStateKey]; !exists {
		return fmt.Errorf("solidified task state is missing from S3 after backend restart")
	}
	if _, exists := afterObjects[backendGCStateKey]; !exists {
		return fmt.Errorf("GC coordinator state is missing from S3 after backend restart")
	}
	gcRecovery, err := verifyBackendS3GCRecovery(ctx, environment)
	if err != nil {
		return err
	}
	resumedBlobCacheKey := testkit.String(resumeEvidence["cacheObjectKey"])
	if err := compareObjectSnapshots(beforeObjects, afterObjects, resumedBlobCacheKey); err != nil {
		return err
	}
	return support.WriteResult(environment, map[string]any{
		"secretName": secretName, "secretKeySHA256": fingerprint,
		"bucket": bucket, "prefix": backendS3Prefix,
		"multipartThreshold": 5242880, "partSize": 5242880,
		"multipartBlobDigest": multipartBlobDigest, "multipartBlobSize": multipartBlobSize,
		"multipartObjectSize": storedMultipartSize, "multipartETag": multipartETag, "multipartParts": multipartParts,
		"s3ObjectCountBefore": len(beforeObjects), "s3ObjectCountAfter": len(afterObjects),
		"s3ObjectKeysSHA256": snapshotDigest(afterObjects),
		"taskID":             taskID, "taskStatus": testkit.String(taskAfter["status"]),
		"backendS3HTTPMetrics": metrics,
		"gcRecovery":           gcRecovery,
		"rootDigest":           manifestDigest,
		"manifestDigestBefore": support.Digest(manifestBefore), "manifestDigestAfter": support.Digest(manifestAfter),
		"downloadResume": resumeEvidence,
	})
}

func readS3AttemptMetrics(ctx context.Context, environment testkit.CaseContext) (map[string]float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("metrics returned %s", response.Status)
	}
	body, err := testkit.ReadBody(response, 8<<20)
	if err != nil {
		return nil, err
	}
	labels := map[string]string{"domain": "state"}
	values := make(map[string]float64, 2)
	for _, name := range []string{"registry_hub_s3_http_attempts_total", "registry_hub_s3_http_attempt_duration_seconds_count"} {
		value, found, err := testkit.PrometheusSum(body, name, labels)
		if err != nil {
			return nil, err
		}
		if !found || value <= 0 {
			return nil, fmt.Errorf("backend S3 metric %s has no positive state-domain samples", name)
		}
		values[name] = value
	}
	return values, nil
}

func verifyBackendS3Environment(ctx context.Context, environment testkit.CaseContext, secretName string) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	template := testkit.Map(testkit.Map(deployment["spec"])["template"])
	podSpec := testkit.Map(template["spec"])
	var backend map[string]any
	for _, raw := range testkit.Slice(podSpec["containers"]) {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) == "backend" {
			backend = container
			break
		}
	}
	if backend == nil {
		return fmt.Errorf("backend Deployment has no backend container")
	}
	fixtureName := environment.Release + backendS3SecretSuffix
	expected := map[string]string{
		"REGISTRY_HUB_STORAGE_BACKEND":        "s3",
		"REGISTRY_HUB_S3_ENDPOINT":            fmt.Sprintf("http://%s.%s.svc.cluster.local:8080", fixtureName, environment.Namespace),
		"REGISTRY_HUB_S3_REGION":              "us-east-1",
		"REGISTRY_HUB_S3_BUCKET":              "rh-" + environment.RunID,
		"REGISTRY_HUB_S3_PREFIX":              backendS3Prefix,
		"REGISTRY_HUB_S3_FORCE_PATH_STYLE":    "true",
		"REGISTRY_HUB_S3_ALLOW_HTTP":          "true",
		"REGISTRY_HUB_S3_MULTIPART_THRESHOLD": "5242880",
		"REGISTRY_HUB_S3_PART_SIZE":           "5242880",
	}
	actual := make(map[string]string, len(expected))
	for _, raw := range testkit.Slice(backend["env"]) {
		item := testkit.Map(raw)
		name := testkit.String(item["name"])
		if _, wanted := expected[name]; wanted {
			actual[name] = testkit.String(item["value"])
		}
	}
	for name, wanted := range expected {
		if actual[name] != wanted {
			return fmt.Errorf("backend S3 environment %s is %q, want %q", name, actual[name], wanted)
		}
	}
	for _, raw := range testkit.Slice(backend["envFrom"]) {
		secretRef := testkit.Map(testkit.Map(raw)["secretRef"])
		if testkit.String(secretRef["name"]) == secretName {
			return nil
		}
	}
	return fmt.Errorf("backend Deployment does not load the S3 credentials Secret")
}
