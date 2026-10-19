package upstreamauthtransport

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

const publicAuthorityIP = "34.192.215.158"

func configurePublicAuthorityHosts(ctx context.Context, environment testkit.CaseContext) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	spec := testkit.Map(deployment["spec"])
	template := testkit.Map(spec["template"])
	podSpec := testkit.Map(template["spec"])
	wanted := map[string]struct{}{
		"registry-1.docker.io":            {},
		"auth.docker.io":                  {},
		"github.com":                      {},
		"ghcr.io":                         {},
		"tls.registry-hub.test":           {},
		"tls-untrusted.registry-hub.test": {},
	}
	seen := make(map[string]struct{}, len(wanted))
	aliases := testkit.Slice(podSpec["hostAliases"])
	for _, raw := range aliases {
		alias := testkit.Map(raw)
		for _, rawHost := range testkit.Slice(alias["hostnames"]) {
			host := testkit.String(rawHost)
			if _, ok := wanted[host]; !ok {
				continue
			}
			if testkit.String(alias["ip"]) != publicAuthorityIP {
				return fmt.Errorf("upstream fixture authority %s already has a different host alias", host)
			}
			seen[host] = struct{}{}
		}
	}
	missing := make([]string, 0, len(wanted)-len(seen))
	for host := range wanted {
		if _, ok := seen[host]; !ok {
			missing = append(missing, host)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		aliases = append(aliases, map[string]any{"ip": publicAuthorityIP, "hostnames": missing})
		podSpec["hostAliases"] = aliases
		template["spec"] = podSpec
		spec["template"] = template
		deployment["spec"] = spec
		encoded, err := json.Marshal(deployment)
		if err != nil {
			return err
		}
		if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "replace", "-f", "-"); err != nil {
			return fmt.Errorf("configure public upstream fixture DNS aliases: %w", err)
		}
		if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
			return fmt.Errorf("backend did not become ready with controlled upstream DNS: %w", err)
		}
	}
	return waitBackendServiceEndpoint(ctx, environment)
}

func waitBackendServiceEndpoint(ctx context.Context, environment testkit.CaseContext) error {
	deadline := time.NewTimer(45 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		pods, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "get", "pods", "-l",
			"app.kubernetes.io/instance="+environment.Release+",app.kubernetes.io/component=backend", "-o", "json")
		if err == nil {
			var podList map[string]any
			if json.Unmarshal(pods.Stdout, &podList) == nil {
				readyIPs := make(map[string]struct{})
				for _, rawPod := range testkit.Slice(podList["items"]) {
					pod := testkit.Map(rawPod)
					status := testkit.Map(pod["status"])
					for _, rawCondition := range testkit.Slice(status["conditions"]) {
						condition := testkit.Map(rawCondition)
						if testkit.String(condition["type"]) == "Ready" && testkit.String(condition["status"]) == "True" {
							if ip := testkit.String(status["podIP"]); ip != "" {
								readyIPs[ip] = struct{}{}
							}
						}
					}
				}
				endpoints, endpointErr := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", environment.Fullname)
				if endpointErr == nil {
					for _, rawSubset := range testkit.Slice(endpoints["subsets"]) {
						for _, rawAddress := range testkit.Slice(testkit.Map(rawSubset)["addresses"]) {
							if _, ok := readyIPs[testkit.String(testkit.Map(rawAddress)["ip"])]; ok {
								return nil
							}
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("backend Service endpoint did not converge on a Ready Pod")
		case <-ticker.C:
		}
	}
}

func waitProxyReady(ctx context.Context, environment testkit.CaseContext) error {
	name := environment.Release + proxySuffix
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "wait", "--for=condition=Ready", "pod/"+name, "--timeout=300s"); err != nil {
		logs, _ := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+name, "-c", "proxy", "--tail=40")
		return fmt.Errorf("upstream proxy fixture did not become ready: %w: %s", err, strings.TrimSpace(string(logs.Stdout)))
	}
	deadline := time.NewTimer(180 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		endpoints, err := testkit.KubectlGet(ctx, environment.Namespace, "endpoints", name)
		if err == nil {
			for _, subset := range testkit.Slice(endpoints["subsets"]) {
				for _, address := range testkit.Slice(testkit.Map(subset)["addresses"]) {
					if testkit.String(testkit.Map(address)["ip"]) != "" {
						return nil
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			logs, _ := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "logs", "pod/"+name, "-c", "proxy", "--tail=40")
			return fmt.Errorf("upstream proxy fixture Service has no ready endpoint: %s", strings.TrimSpace(string(logs.Stdout)))
		case <-ticker.C:
		}
	}
}
