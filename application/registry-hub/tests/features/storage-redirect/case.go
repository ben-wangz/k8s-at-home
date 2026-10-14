package storageredirect

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

const registryS3CredentialsSuffix = "-external-registry-s3"

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "feature-storage-redirect")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	serviceName := environment.Release + registryS3CredentialsSuffix
	dnsName := serviceName + "." + environment.Namespace + ".svc.cluster.local"
	endpoint := "https://" + dnsName + ":8080"
	serviceIP, err := verifyPrivateFixtureService(ctx, environment, serviceName)
	if err != nil {
		return err
	}
	if err := verifyRedirectConfiguration(ctx, environment, endpoint); err != nil {
		return err
	}
	registry := support.NewRegistryClient(environment)
	repository := support.Repository(environment.CaseID, environment.RunID)
	fixture, err := support.BuildImageFixture(ctx, registry, repository, "redirect-probe", environment.RunID)
	if err != nil {
		return err
	}
	before, err := upstreamRequests(ctx, environment)
	if err != nil {
		return err
	}
	response, err := registry.Get(ctx, fixture.Repository, "/blobs/"+fixture.ConfigDigest, nil)
	if err != nil {
		return err
	}
	status := response.StatusCode
	location := response.Header.Get("Location")
	body, err := testkit.ReadBody(response, 64<<10)
	if err != nil {
		return err
	}
	if status != http.StatusBadGateway {
		return fmt.Errorf("private storage redirect returned %d, want 502", status)
	}
	if location != "" || responseLeaksStorageTarget(body, dnsName) {
		return fmt.Errorf("storage redirect error exposed its target or signed URL")
	}
	after, err := upstreamRequests(ctx, environment)
	if err != nil {
		return err
	}
	if before != after {
		return fmt.Errorf("storage redirect error triggered an upstream request")
	}
	return support.WriteResult(environment, map[string]any{
		"endpoint": endpoint, "responseStatus": status,
		"fixtureServiceIP": serviceIP, "fixtureServiceIPPrivate": true,
		"upstreamRequestsBefore": before, "upstreamRequestsAfter": after,
		"locationHeader": "absent", "signedURLLeak": false,
		"repository": fixture.Repository, "manifestDigest": fixture.ManifestDigest,
	})
}

func verifyPrivateFixtureService(ctx context.Context, environment testkit.CaseContext, serviceName string) (string, error) {
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return "", err
	}
	spec := testkit.Map(service["spec"])
	if testkit.String(spec["type"]) != "ClusterIP" {
		return "", fmt.Errorf("storage redirect fixture Service is not ClusterIP")
	}
	address, err := netip.ParseAddr(testkit.String(spec["clusterIP"]))
	if err != nil || !address.IsPrivate() {
		return "", fmt.Errorf("storage redirect fixture Service does not have a private ClusterIP")
	}
	return address.String(), nil
}

func verifyRedirectConfiguration(ctx context.Context, environment testkit.CaseContext, endpoint string) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	containers := testkit.Slice(testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["template"])["spec"])["containers"])
	for _, raw := range containers {
		container := testkit.Map(raw)
		if testkit.String(container["name"]) != "backend" {
			continue
		}
		if envValue(container, "REGISTRY_HUB_DISTRIBUTION_REDIRECT_ENDPOINTS") != `["`+endpoint+`"]` {
			return fmt.Errorf("backend Deployment has an unexpected Distribution redirect allowlist")
		}
		configMap, err := testkit.KubectlGet(ctx, environment.Namespace, "configmap", environment.Fullname+"-registry-config")
		if err != nil {
			return err
		}
		config := testkit.String(testkit.Map(configMap["data"])["config.yml"])
		if !strings.Contains(config, "redirect:\n    disable: false") || !strings.Contains(config, "regionendpoint: \""+endpoint+"\"") {
			return fmt.Errorf("Distribution redirect or S3 endpoint is not enabled as configured")
		}
		return nil
	}
	return fmt.Errorf("backend Deployment has no backend container")
}

func envValue(container map[string]any, name string) string {
	for _, raw := range testkit.Slice(container["env"]) {
		item := testkit.Map(raw)
		if testkit.String(item["name"]) == name {
			return testkit.String(item["value"])
		}
	}
	return ""
}

func upstreamRequests(ctx context.Context, environment testkit.CaseContext) (float64, error) {
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/metrics", nil, nil)
	if err != nil {
		return 0, err
	}
	metrics, err := testkit.ReadBody(response, 4<<20)
	if err != nil {
		return 0, err
	}
	var count float64
	for _, line := range strings.Split(string(metrics), "\n") {
		if strings.HasPrefix(line, "#") || !strings.HasPrefix(line, "registry_hub_upstream_requests_total") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("upstream request metric is malformed")
		}
		value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("upstream request metric is not numeric")
		}
		count += value
	}
	return count, nil
}

func responseLeaksStorageTarget(body []byte, dnsName string) bool {
	text := strings.ToLower(string(body))
	for _, marker := range []string{strings.ToLower(dnsName), "x-amz-signature", "x-amz-credential", "awsaccesskeyid", "signature="} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
