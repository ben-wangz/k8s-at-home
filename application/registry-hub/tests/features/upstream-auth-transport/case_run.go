package upstreamauthtransport

import (
	"context"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"strings"
)

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secretName := environment.Release + secretSuffix
	secret, data, err := support.GetSecret(ctx, environment.Namespace, secretName)
	if err != nil {
		return err
	}
	if secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" {
		return fmt.Errorf("upstream config Secret ownership is invalid")
	}
	password, bearerToken, fingerprint, err := validateSecret(data, environment.Release, environment.Namespace)
	if err != nil {
		return err
	}
	if err := createProxyFixture(ctx, environment, secretName); err != nil {
		return err
	}
	if err := waitProxyReady(ctx, environment); err != nil {
		return err
	}
	if err := configurePublicAuthorityHosts(ctx, environment); err != nil {
		return err
	}

	registry := support.NewRegistryClient(environment)
	dockerRepo := "docker.io/library/registry-hub-test"
	dockerBody, dockerHeaders, err := support.ReadManifest(ctx, registry, dockerRepo, "authorized", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("Docker Hub authority pull failed: %w", err)
	}
	if err := validateManifestResponse(dockerBody, dockerHeaders); err != nil {
		return fmt.Errorf("Docker Hub authority response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "docker.io", "library/registry-hub-test", "authorized"); err != nil {
		return fmt.Errorf("Docker Hub authority pull did not solidify: %w", err)
	}
	challengeBody, challengeHeaders, err := support.ReadManifest(ctx, registry, dockerRepo, "challenge", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("Docker Hub Bearer challenge flow failed: %w", err)
	}
	if err := validateManifestResponse(challengeBody, challengeHeaders); err != nil {
		return fmt.Errorf("Docker Hub challenge response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "docker.io", "library/registry-hub-test", "challenge"); err != nil {
		return fmt.Errorf("Docker Hub Bearer challenge pull did not solidify: %w", err)
	}
	tokenRetryBody, tokenRetryHeaders, err := support.ReadManifest(ctx, registry, dockerRepo, "token-retry", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("token endpoint retry pull failed: %w", err)
	}
	if err := validateManifestResponse(tokenRetryBody, tokenRetryHeaders); err != nil {
		return fmt.Errorf("token endpoint retry response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "docker.io", "library/registry-hub-test", "token-retry"); err != nil {
		return fmt.Errorf("token endpoint retry pull did not solidify: %w", err)
	}

	githubRepo := "github.com/registry-hub-test/fixture"
	githubBody, githubHeaders, err := support.ReadManifest(ctx, registry, githubRepo, "isolated", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("second authority pull failed: %w", err)
	}
	if err := validateManifestResponse(githubBody, githubHeaders); err != nil {
		return fmt.Errorf("second authority response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "github.com", "registry-hub-test/fixture", "isolated"); err != nil {
		return fmt.Errorf("second authority pull did not solidify: %w", err)
	}
	privateResponse, err := registry.Get(ctx, githubRepo, "/manifests/private", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return fmt.Errorf("private upstream manifest request failed before receiving a response: %w", err)
	}
	privateStatus := privateResponse.StatusCode
	privateChallenge := privateResponse.Header.Get("WWW-Authenticate")
	privateEnvelope, privateDecodeErr := support.DecodeError(privateResponse)
	if privateDecodeErr != nil || privateStatus != http.StatusUnauthorized || privateChallenge != "" || len(testkit.Slice(privateEnvelope["errors"])) != 1 ||
		testkit.String(testkit.Map(testkit.Slice(privateEnvelope["errors"])[0])["code"]) != "UNAUTHORIZED" {
		return fmt.Errorf("anonymous private manifest response status=%d challenge=%q envelope=%v error=%v", privateStatus, privateChallenge, privateEnvelope, privateDecodeErr)
	}
	privateTask, err := waitForTaskStatus(ctx, environment, "github.com", "registry-hub-test/fixture", "private", "failed")
	if err != nil {
		return fmt.Errorf("private image rejection task did not fail as expected: %w", err)
	}
	if testkit.String(privateTask["errorClass"]) != "upstream" || testkit.String(privateTask["rootDigest"]) != "" || privateTask["rootPublished"] == true {
		return fmt.Errorf("private image failure task advanced a root or has the wrong error class: %v", privateTask)
	}
	if !strings.EqualFold(support.Digest(dockerBody), support.Digest(githubBody)) {
		return fmt.Errorf("controlled authorities returned different manifest bytes")
	}
	ghcrRepo := "ghcr.io/registry-hub-test/fixture"
	ghcrBody, ghcrHeaders, err := support.ReadManifest(ctx, registry, ghcrRepo, "bearer", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("GHCR Bearer authority pull failed: %w", err)
	}
	if err := validateManifestResponse(ghcrBody, ghcrHeaders); err != nil {
		return fmt.Errorf("GHCR Bearer authority response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "ghcr.io", "registry-hub-test/fixture", "bearer"); err != nil {
		return fmt.Errorf("GHCR Bearer pull did not solidify: %w", err)
	}
	if !strings.EqualFold(support.Digest(dockerBody), support.Digest(ghcrBody)) {
		return fmt.Errorf("controlled authorities returned different manifest bytes")
	}
	tlsRepository := "tls.registry-hub.test/registry-hub-test/fixture"
	tlsBody, tlsHeaders, err := support.ReadManifest(ctx, registry, tlsRepository, "trusted", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("custom-CA HTTPS authority pull failed: %w", err)
	}
	if err := validateManifestResponse(tlsBody, tlsHeaders); err != nil {
		return fmt.Errorf("custom-CA HTTPS response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "tls.registry-hub.test", "registry-hub-test/fixture", "trusted"); err != nil {
		return fmt.Errorf("custom-CA HTTPS pull did not solidify: %w", err)
	}
	untrustedRepository := "tls-untrusted.registry-hub.test/registry-hub-test/fixture"
	if _, _, err := support.ReadManifest(ctx, registry, untrustedRepository, "untrusted", support.OCIManifestMediaType); err == nil || !strings.Contains(err.Error(), "502 Bad Gateway") {
		return fmt.Errorf("untrusted TLS authority result = %v, want 502", err)
	}
	redirectBody, redirectHeaders, err := support.ReadManifest(ctx, registry, dockerRepo, "redirected", support.OCIManifestMediaType)
	if err != nil {
		return fmt.Errorf("cross-authority redirect failed: %w", err)
	}
	if err := validateManifestResponse(redirectBody, redirectHeaders); err != nil {
		return fmt.Errorf("cross-authority redirect response is invalid: %w", err)
	}
	if err := waitForSolidifiedTask(ctx, environment, "docker.io", "library/registry-hub-test", "redirected"); err != nil {
		return fmt.Errorf("cross-authority redirect did not solidify before the cache check: %w", err)
	}
	cachedBody, _, err := support.ReadManifest(ctx, registry, dockerRepo, "redirected", support.OCIManifestMediaType)
	if err != nil || string(cachedBody) != string(redirectBody) {
		return fmt.Errorf("cross-authority redirected manifest was not served from cache: %v", err)
	}
	ssrfResponse, err := registry.Get(ctx, dockerRepo, "/manifests/ssrf", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	if ssrfResponse.StatusCode != http.StatusBadGateway {
		ssrfResponse.Body.Close()
		return fmt.Errorf("private redirect returned %s, want 502", ssrfResponse.Status)
	}
	ssrfResponse.Body.Close()
	downgradeResponse, err := registry.Get(ctx, tlsRepository, "/manifests/downgrade", map[string]string{"Accept": support.OCIManifestMediaType})
	if err != nil {
		return err
	}
	if downgradeResponse.StatusCode != http.StatusBadGateway {
		downgradeResponse.Body.Close()
		return fmt.Errorf("HTTPS-to-HTTP redirect returned %s, want 502", downgradeResponse.Status)
	}
	downgradeResponse.Body.Close()
	tokenDeadlineResults, err := verifyTokenRequestDeadline(ctx, environment, registry, dockerRepo)
	if err != nil {
		return err
	}

	tokenRetryRequests, err := verifyFixtureRequestLogs(ctx, environment, password, bearerToken)
	if err != nil {
		return err
	}
	digest := support.Digest(dockerBody)
	return support.WriteResult(environment, map[string]any{
		"secretName":             secretName,
		"secretKeySHA256":        fingerprint,
		"dockerAuthority":        "registry-1.docker.io",
		"dockerManifestDigest":   digest,
		"dockerChallengeDigest":  support.Digest(challengeBody),
		"tokenServiceScope":      "repository:library/registry-hub-test:pull",
		"secondAuthority":        "github.com",
		"crossAuthorityAuth":     "absent",
		"bearerAuthority":        "ghcr.io",
		"bearerManifestDigest":   support.Digest(ghcrBody),
		"redirectManifestDigest": support.Digest(redirectBody),
		"tlsManifestDigest":      support.Digest(tlsBody),
		"redirectCacheRequests":  1,
		"privateRedirectStatus":  http.StatusBadGateway,
		"privateImageStatus":     privateStatus,
		"privateImageErrorClass": testkit.String(privateTask["errorClass"]),
		"untrustedTLSStatus":     http.StatusBadGateway,
		"downgradeStatus":        http.StatusBadGateway,
		"publicAuthorityIP":      publicAuthorityIP,
		"proxyEvidence":          "fixture logs contain authority, path, auth state and non-secret pull scope",
		"tokenDeadline":          tokenDeadlineResults,
		"tokenRetryRequests":     tokenRetryRequests,
		"tokenRetryDigest":       support.Digest(tokenRetryBody),
	})
}

func waitForSolidifiedTask(ctx context.Context, environment testkit.CaseContext, authority, repository, reference string) error {
	_, err := waitForTaskStatus(ctx, environment, authority, repository, reference, "solidified")
	return err
}
