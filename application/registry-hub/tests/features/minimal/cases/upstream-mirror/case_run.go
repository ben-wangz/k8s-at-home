package upstreammirror

import (
	"context"
	"errors"
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

func Run(ctx context.Context, environment testkit.CaseContext) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, captureLatencyEvidence(context.WithoutCancel(ctx), environment, "failure"))
		}
	}()
	var run *mirrorRun
	if err := testkit.RunPhase(environment, "fixture-setup", func() error {
		var err error
		run, err = setupMirrorRun(ctx, environment)
		return err
	}); err != nil {
		return err
	}
	result := make(map[string]any)
	phases := []mirrorPhase{
		{"local-first", func() (map[string]any, error) { return verifyLocalFirst(run) }},
		{"successful-pull", func() (map[string]any, error) { return verifySuccessfulPull(run) }},
	}
	for _, phase := range phases {
		values, err := runMirrorPhase(environment, phase)
		if err != nil {
			return err
		}
		mergeMirrorResults(result, values)
	}
	var discoveryDigest string
	if err := testkit.RunPhase(environment, "discovery", func() error {
		values, digest, err := verifyDiscoveryFlow(run)
		discoveryDigest = digest
		mergeMirrorResults(result, values)
		return err
	}); err != nil {
		return err
	}
	for _, phase := range []mirrorPhase{
		{"compatibility", func() (map[string]any, error) { return verifyCompatibilityFallbacks(run, discoveryDigest) }},
		{"client-accept-head", func() (map[string]any, error) { return verifyClientAcceptAndHead(run) }},
		{"range-request", func() (map[string]any, error) { return verifyUpstreamRangeRequest(run) }},
		{"digest-miss", func() (map[string]any, error) { return verifyDigestAndMissBehavior(run) }},
		{"supplemental", func() (map[string]any, error) { return verifySupplementalCases(run) }},
		{"multipart", func() (map[string]any, error) { return verifyMultipartProbe(run) }},
		{"limits", func() (map[string]any, error) { return verifyLimitCases(run) }},
	} {
		values, err := runMirrorPhase(environment, phase)
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
