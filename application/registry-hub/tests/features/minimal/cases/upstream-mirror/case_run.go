package upstreammirror

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"strings"
)

type mirrorRun struct {
	context            context.Context
	environment        testkit.CaseContext
	secretName         string
	secretData         map[string][]byte
	registry           *support.RegistryClient
	localRepository    string
	remoteRepository   string
	upstreamRepository string
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	run, err := setupMirrorRun(ctx, environment)
	if err != nil {
		return err
	}
	result := make(map[string]any)
	phases := []func() (map[string]any, error){
		func() (map[string]any, error) { return verifyLocalFirst(run) },
		func() (map[string]any, error) { return verifySuccessfulPull(run) },
	}
	for _, phase := range phases {
		values, err := phase()
		if err != nil {
			return err
		}
		mergeMirrorResults(result, values)
	}
	discoveryResults, discoveryDigest, err := verifyDiscoveryFlow(run)
	if err != nil {
		return err
	}
	mergeMirrorResults(result, discoveryResults)
	for _, phase := range []func() (map[string]any, error){
		func() (map[string]any, error) { return verifyCompatibilityFallbacks(run, discoveryDigest) },
		func() (map[string]any, error) { return verifyClientAcceptAndHead(run) },
		func() (map[string]any, error) { return verifyUpstreamRangeRequest(run) },
		func() (map[string]any, error) { return verifyDigestAndMissBehavior(run) },
		func() (map[string]any, error) { return verifySupplementalCases(run) },
		func() (map[string]any, error) { return verifyMultipartProbe(run) },
		func() (map[string]any, error) { return verifyLimitCases(run) },
	} {
		values, err := phase()
		if err != nil {
			return err
		}
		mergeMirrorResults(result, values)
	}
	if err := restoreBackendRuntimeDefaults(ctx, environment); err != nil {
		return fmt.Errorf("restore backend runtime defaults: %w", err)
	}
	result["runtimeOverridesRestored"] = true
	return support.WriteResult(environment, result)
}

func setupMirrorRun(ctx context.Context, environment testkit.CaseContext) (*mirrorRun, error) {
	secret, secretData, err := prepareFixtureSecret(ctx, environment)
	if err != nil {
		return nil, err
	}
	registrySecret, registryData, err := support.GetSecret(ctx, environment.Namespace, environment.Fullname+"-registry-internal")
	if err != nil {
		return nil, err
	}
	if registrySecret.UID == "" || len(registryData["ca.crt"]) == 0 {
		return nil, fmt.Errorf("internal Registry CA is unavailable")
	}
	caBundle := append(append(append([]byte(nil), registryData["ca.crt"]...), '\n'), secretData["UPSTREAM_TLS_CA_CERT"]...)
	fixtureSources, err := upstreamauthtransport.FixtureSources()
	if err != nil {
		return nil, err
	}
	if err := upstreamauthtransport.CreateProxyFixture(ctx, environment, secret.Name, fixtureSources, map[string]string{"ca-bundle.crt": string(caBundle)}); err != nil {
		return nil, err
	}
	if err := upstreamauthtransport.WaitProxyReady(ctx, environment); err != nil {
		return nil, err
	}
	if err := configureBackendProxyEnvironment(ctx, environment); err != nil {
		return nil, err
	}
	const upstreamRepository = "registry-hub-test/fixture"
	return &mirrorRun{
		context: ctx, environment: environment, secretName: secret.Name, secretData: secretData,
		registry:           support.NewRegistryClient(environment),
		localRepository:    support.LocalRepository(environment.CaseID, environment.RunID),
		remoteRepository:   "github.com/" + upstreamRepository,
		upstreamRepository: upstreamRepository,
	}, nil
}

func verifyLocalFirst(run *mirrorRun) (map[string]any, error) {
	fixture, err := support.BuildImageFixture(run.context, run.registry, run.localRepository, "minimal", run.environment.RunID)
	if err != nil {
		return nil, fmt.Errorf("create local-first fixture: %w", err)
	}
	manifest, headers, err := support.ReadManifest(run.context, run.registry, run.localRepository, "minimal", support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("local-first manifest read failed: %w", err)
	}
	if !strings.EqualFold(support.Digest(manifest), headers.Get("Docker-Content-Digest")) {
		return nil, fmt.Errorf("local-first manifest digest header does not match its bytes")
	}
	if string(manifest) != string(fixture.Manifest) {
		return nil, fmt.Errorf("local-first manifest bytes differ from the fixture")
	}
	logs, err := fixtureLogs(run.context, run.environment)
	if err != nil {
		return nil, err
	}
	if strings.Contains(logs, "/manifests/") {
		return nil, fmt.Errorf("local manifest hit reached the upstream fixture")
	}
	if strings.Contains(logs, "path=/v2/") {
		return nil, fmt.Errorf("local writes or reads reached an upstream Registry API")
	}
	queueResults, err := verifyQueueSaturation(run.context, run.environment, run.registry, run.localRepository, manifest)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"fixtureSecret": run.secretName, "fixtureSecretKeySHA256": support.SecretHashes(run.secretData),
		"localRepository": run.localRepository, "localManifestDigest": fixture.ManifestDigest,
		"localManifestBytesMatch": string(manifest) == string(fixture.Manifest), "localUpstreamRequests": 0,
	}
	mergeMirrorResults(result, queueResults)
	return result, nil
}

func verifySuccessfulPull(run *mirrorRun) (map[string]any, error) {
	waiterResults, err := verifySharedBlobWaiter(run.context, run.environment, run.registry, run.remoteRepository)
	if err != nil {
		return nil, err
	}
	manifestBytes, headers, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, "minimal-success", support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("successful upstream manifest read failed: %w", err)
	}
	digest := support.Digest(manifestBytes)
	if !strings.EqualFold(digest, headers.Get("Docker-Content-Digest")) {
		return nil, fmt.Errorf("successful upstream manifest digest header does not match its bytes")
	}
	var manifest support.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil || manifest.Config == nil {
		return nil, fmt.Errorf("successful upstream manifest has no valid config descriptor")
	}
	config := []byte("{}")
	if manifest.Config.Digest != support.Digest(config) || manifest.Config.Size != len(config) {
		return nil, fmt.Errorf("successful upstream config descriptor does not match fixture bytes")
	}
	task, err := waitForTaskStatus(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-success", "waiting_dependencies")
	if err != nil {
		return nil, fmt.Errorf("manifest discovery did not wait for the client config request: %w", err)
	}
	configRequestsBefore, err := countFixturePath(run.context, run.environment, "/v2/registry-hub-test/fixture/blobs/"+manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	if configRequestsBefore != 0 {
		return nil, fmt.Errorf("manifest discovery fetched an unrequested config %d times", configRequestsBefore)
	}
	clientConfig, err := support.ReadBlob(run.context, run.registry, run.remoteRepository, manifest.Config.Digest)
	if err != nil || string(clientConfig) != string(config) {
		return nil, fmt.Errorf("client config request did not return the expected bytes: %v", err)
	}
	configRequestsAfter, err := countFixturePath(run.context, run.environment, "/v2/registry-hub-test/fixture/blobs/"+manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	if configRequestsAfter != 1 {
		return nil, fmt.Errorf("client config request reached upstream %d times, want once", configRequestsAfter)
	}
	task, err = waitForSolidifiedTask(run.context, run.environment, "github.com", run.upstreamRepository, "minimal-success")
	if err != nil {
		return nil, err
	}
	if testkit.String(task["rootDigest"]) != digest || testkit.String(task["status"]) != "solidified" || task["rootPublished"] != true {
		return nil, fmt.Errorf("successful upstream task did not publish its manifest root")
	}
	requestCount, err := countFixtureRequests(run.context, run.environment, "minimal-success")
	if err != nil {
		return nil, err
	}
	if requestCount != 1 {
		return nil, fmt.Errorf("successful upstream manifest requests = %d, want 1", requestCount)
	}
	localManifest, localHeaders, err := support.ReadManifest(run.context, run.registry, run.remoteRepository, "minimal-success", support.OCIManifestMediaType)
	if err != nil {
		return nil, fmt.Errorf("solidified manifest is not locally readable: %w", err)
	}
	if string(localManifest) != string(manifestBytes) || !strings.EqualFold(support.Digest(localManifest), localHeaders.Get("Docker-Content-Digest")) {
		return nil, fmt.Errorf("solidified manifest bytes or digest changed")
	}
	localConfig, err := support.ReadBlob(run.context, run.registry, run.remoteRepository, manifest.Config.Digest)
	if err != nil {
		return nil, fmt.Errorf("solidified config blob is not locally readable: %w", err)
	}
	if string(localConfig) != string(config) {
		return nil, fmt.Errorf("solidified config blob bytes changed")
	}
	requestCountAfterLocalRead, err := countFixtureRequests(run.context, run.environment, "minimal-success")
	if err != nil {
		return nil, err
	}
	if requestCountAfterLocalRead != requestCount {
		return nil, fmt.Errorf("local reads reached upstream again: manifest requests changed from %d to %d", requestCount, requestCountAfterLocalRead)
	}
	result := map[string]any{
		"successfulRepository": run.remoteRepository, "successfulReference": "minimal-success",
		"successfulManifestDigest": digest, "solidifiedTaskID": testkit.String(task["id"]),
		"solidifiedTaskStatus": testkit.String(task["status"]), "solidifiedRootPublished": task["rootPublished"],
		"importedManifestBytesMatch": string(localManifest) == string(manifestBytes),
		"importedConfigDigest":       manifest.Config.Digest, "importedConfigBytesMatch": string(localConfig) == string(config),
		"successfulManifestRequests": requestCount,
	}
	mergeMirrorResults(result, waiterResults)
	return result, nil
}
