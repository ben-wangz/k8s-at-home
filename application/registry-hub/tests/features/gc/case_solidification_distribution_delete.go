package gc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type distributionCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func directDistributionDeleteBlob(ctx context.Context, environment testkit.CaseContext, repository, digest string) (evidence map[string]any, err error) {
	serviceName := environment.Fullname + "-registry"
	service, err := testkit.KubectlGet(ctx, environment.Namespace, "service", serviceName)
	if err != nil {
		return nil, err
	}
	spec := testkit.Map(service["spec"])
	clusterIP := testkit.String(spec["clusterIP"])
	if net.ParseIP(clusterIP) == nil || !serviceHasRegistryPort(spec) {
		return nil, fmt.Errorf("Distribution Service endpoint is invalid")
	}
	nodeName, err := distributionNodeName(ctx, environment, serviceName)
	if err != nil {
		return nil, err
	}

	secret, secretData, err := support.GetSecret(ctx, environment.Namespace, environment.Fullname+"-registry-internal")
	if err != nil {
		return nil, err
	}
	if secret.Labels["registry-hub.io/instance"] != environment.Fullname || secret.Labels["registry-hub.io/managed-by"] != "internal-initializer" {
		return nil, fmt.Errorf("internal Distribution Secret ownership is invalid")
	}
	var credential distributionCredential
	if err := json.Unmarshal(secretData["config.json"], &credential); err != nil || credential.Username != "registry-hub-backend" || credential.Password == "" || len(secretData["ca.crt"]) == 0 {
		return nil, fmt.Errorf("internal Distribution credentials or CA are invalid")
	}
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return nil, err
	}
	selector := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"])
	if len(selector) == 0 {
		return nil, fmt.Errorf("backend Deployment has no selector for the Distribution probe")
	}
	internalRepository := internalDistributionRepository(repository)
	probeName := environment.Release + "-dist-delete"
	probeImage := strings.TrimSpace(os.Getenv("REGISTRY_HUB_TEST_PROBE_IMAGE"))
	if probeImage == "" {
		probeImage = "container-registry-mirror.internal.home.geekcity.tech:32443/docker.io/curlimages/curl:8.10.1"
	}
	manifest, err := directDistributionProbeManifest(environment, selector, probeName, probeImage, serviceName, clusterIP, nodeName, internalRepository, digest)
	if err != nil {
		return nil, err
	}
	if _, err := testkit.CommandInput(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		return nil, fmt.Errorf("create direct Distribution probe Pod: %w", err)
	}
	probeCreated := true
	defer func() {
		if !probeCreated {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		_, cleanupErr := testkit.Command(cleanupCtx, "kubectl", "-n", environment.Namespace, "delete", "pod", probeName, "--ignore-not-found", "--wait=true", "--timeout=75s")
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("delete direct Distribution probe Pod: %w", cleanupErr))
			return
		}
		if evidence != nil {
			evidence["probePodCleaned"] = true
		}
	}()

	statuses, probeOutput, err := waitForDirectDistributionProbe(ctx, environment, probeName)
	if err != nil {
		return nil, directDistributionProbeFailure(ctx, environment, internalRepository, digest, nil, probeOutput, err)
	}
	if statuses[0] != "200/0" || statuses[1] != "200/0" || statuses[2] != "202/0" || statuses[3] != "404/0" {
		return nil, directDistributionProbeFailure(ctx, environment, internalRepository, digest, statuses, probeOutput, nil)
	}
	evidence = map[string]any{
		"service": serviceName, "probePod": probeName,
		"distributionRepository": internalRepository,
		"distributionV2Result":   statuses[0], "readBeforeDeleteResult": statuses[1],
		"directDeleteResult": statuses[2], "readAfterDeleteResult": statuses[3],
		"apiPath": "Distribution Service /v2 blob API",
	}
	return evidence, nil
}

func directDistributionProbeFailure(ctx context.Context, environment testkit.CaseContext, repository, digest string, statuses []string, probeOutput string, cause error) error {
	requests, logErr := directDistributionBlobRequestEvidence(ctx, environment, digest)
	evidence := map[string]any{
		"distributionRepository": repository,
		"blobDigest":             digest,
		"probeResults":           statuses,
		"probeOutput":            strings.TrimSpace(probeOutput),
		"distributionRequests":   requests,
	}
	if logErr != nil {
		evidence["distributionLogError"] = logErr.Error()
	}
	encoded, _ := json.Marshal(evidence)
	if cause != nil {
		return fmt.Errorf("direct Distribution probe failed: %w; evidence: %s", cause, encoded)
	}
	return fmt.Errorf("direct Distribution /v2/ and blob GET/DELETE/GET results = %v, want [200/0 200/0 202/0 404/0]; evidence: %s", statuses, encoded)
}

func directDistributionBlobRequestEvidence(ctx context.Context, environment testkit.CaseContext, digest string) ([]string, error) {
	lines, err := distributionLogs(ctx, environment, time.Time{})
	if err != nil {
		return nil, err
	}
	requests := make([]string, 0, 8)
	for _, line := range lines {
		if !strings.Contains(line, `http.request.uri=`) || !strings.Contains(line, digest) {
			continue
		}
		if len(line) > 4096 {
			line = line[:4096] + "[truncated]"
		}
		requests = append(requests, line)
		if len(requests) == 50 {
			break
		}
	}
	return requests, nil
}

func internalDistributionRepository(repository string) string {
	authority, remainder, found := strings.Cut(repository, "/")
	if !found || !strings.ContainsAny(authority, ".:") && !strings.HasPrefix(authority, "e") && authority != "localhost" {
		return repository
	}
	var encoded strings.Builder
	encoded.WriteByte('e')
	for _, character := range []byte(authority) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			encoded.WriteByte(character)
			continue
		}
		encoded.WriteByte('_')
		encoded.WriteString(hex.EncodeToString([]byte{character}))
	}
	return encoded.String() + "/" + remainder
}

func serviceHasRegistryPort(spec map[string]any) bool {
	for _, raw := range testkit.Slice(spec["ports"]) {
		port := testkit.Map(raw)
		if testkit.Int(port["port"]) == 5000 && testkit.String(port["name"]) == "https-registry" {
			return true
		}
	}
	return false
}

func distributionNodeName(ctx context.Context, environment testkit.CaseContext, serviceName string) (string, error) {
	endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", serviceName)
	if err != nil {
		return "", err
	}
	for _, subset := range testkit.Slice(endpoints["subsets"]) {
		for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
			if nodeName := testkit.String(testkit.Map(address)["nodeName"]); nodeName != "" {
				return nodeName, nil
			}
		}
	}
	return "", fmt.Errorf("Distribution endpoints do not expose a node name")
}
