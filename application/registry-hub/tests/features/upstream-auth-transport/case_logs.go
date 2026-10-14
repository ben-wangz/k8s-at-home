package upstreamauthtransport

import (
	"context"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func verifyFixtureRequestLogs(ctx context.Context, environment testkit.CaseContext, password, bearerToken string) (int, error) {
	logs, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+environment.Release+proxySuffix, "-c", "proxy", "--tail=200")
	if err != nil {
		return 0, err
	}
	text := string(logs.Stdout)
	if strings.Contains(text, password) || strings.Contains(text, bearerToken) {
		return 0, fmt.Errorf("upstream fixture logs exposed the configured credential")
	}
	for _, marker := range []string{
		"host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/authorized auth=basic-matched",
		"host=auth.docker.io path=/token auth=basic-matched scope=repository:library/registry-hub-test:pull",
		"host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/challenge auth=bearer-matched",
		"host=github.com path=/v2/registry-hub-test/fixture/manifests/isolated auth=absent",
		"host=github.com path=/v2/registry-hub-test/fixture/manifests/private auth=absent",
		"host=ghcr.io path=/v2/registry-hub-test/fixture/manifests/bearer auth=bearer-matched",
		"host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/redirected auth=basic-matched",
		"host=github.com path=/v2/registry-hub-test/fixture/manifests/redirected auth=absent",
		"host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/ssrf auth=basic-matched",
		"host=tls.registry-hub.test path=/v2/registry-hub-test/fixture/manifests/trusted auth=absent",
		"host=tls.registry-hub.test path=/v2/registry-hub-test/fixture/manifests/downgrade auth=absent",
	} {
		if !strings.Contains(text, marker) {
			return 0, fmt.Errorf("upstream fixture did not record expected request state %q", marker)
		}
	}
	if strings.Count(text, "path=/v2/library/registry-hub-test/manifests/redirected auth=basic-matched") != 1 ||
		strings.Count(text, "path=/v2/registry-hub-test/fixture/manifests/redirected auth=absent") != 1 ||
		strings.Contains(text, "host=127.0.0.1") ||
		strings.Contains(text, "path=/v2/registry-hub-test/fixture/manifests/downgraded") ||
		strings.Contains(text, "host=tls-untrusted.registry-hub.test path=/v2/registry-hub-test/fixture/manifests/untrusted") {
		return 0, fmt.Errorf("redirect cache, auth isolation, or private-target rejection evidence is invalid")
	}
	registryRetryMarker := "method=GET host=registry-1.docker.io path=/v2/library/registry-hub-test/manifests/token-retry auth=basic-matched"
	tokenRetryMarker := "method=GET host=auth.docker.io path=/token auth=basic-matched scope=repository:library/registry-hub-test:pull service=token-retry"
	registryRetries, tokenRetries := strings.Count(text, registryRetryMarker), strings.Count(text, tokenRetryMarker)
	if registryRetries != 3 || tokenRetries != 3 {
		return 0, fmt.Errorf("token endpoint retry trace is invalid: registry=%d token=%d", registryRetries, tokenRetries)
	}
	return tokenRetries, nil
}
