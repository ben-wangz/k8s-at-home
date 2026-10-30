package gc

import (
	"context"
	"fmt"
	"net"
	"net/url"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func useBackendPodForGC(ctx context.Context, environment testkit.CaseContext, registry *support.RegistryClient) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	selector := testkit.Map(testkit.Map(testkit.Map(deployment["spec"])["selector"])["matchLabels"])
	pods, err := testkit.KubectlList(ctx, environment.Namespace, "get", "pods")
	if err != nil {
		return err
	}
	for _, pod := range pods {
		metadata, status := testkit.Map(pod.Raw["metadata"]), testkit.Map(pod.Raw["status"])
		if metadata["deletionTimestamp"] != nil || !matchesLabels(testkit.Map(metadata["labels"]), selector) ||
			!hasControllerKind(testkit.Slice(metadata["ownerReferences"]), "ReplicaSet") || testkit.String(status["phase"]) != "Running" {
			continue
		}
		endpoint, err := backendClientAddress(environment.ClientURL, testkit.String(status["podIP"]))
		if err != nil {
			return err
		}
		// The client Service excludes unready Pods while unresolved GC keeps writes closed.
		registry.Base = endpoint
		return nil
	}
	return fmt.Errorf("backend Deployment has no running Pod for GC admission checks")
}

func backendClientAddress(clientURL, podIP string) (string, error) {
	endpoint, err := url.Parse(clientURL)
	if err != nil || endpoint.Hostname() == "" || net.ParseIP(podIP) == nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return "", fmt.Errorf("invalid backend client address or Pod IP")
	}
	port := endpoint.Port()
	if port == "" {
		port = "80"
		if endpoint.Scheme == "https" {
			port = "443"
		}
	}
	endpoint.Host = net.JoinHostPort(podIP, port)
	return endpoint.String(), nil
}
