package environment

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (e *Environment) Check(ctx context.Context, scenario string) error {
	tools := []string{"go", "node", "npm", "helm", "kubectl"}
	if os.Getenv("CLASH_TEST_FIXTURE_IMAGE") == "" || !strings.Contains(os.Getenv("CLASH_TEST_MIHOMO_IMAGE"), "@sha256:") {
		tools = append(tools, "podman")
	}
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %s is missing", tool)
		}
	}
	if e.Context == "" {
		output, err := execute(ctx, e.App, nil, nil, "kubectl", "config", "current-context")
		if err != nil {
			return err
		}
		e.Context = strings.TrimSpace(string(output))
	}
	if e.Context == "" {
		return fmt.Errorf("a Kubernetes context is required")
	}
	if _, err := e.Kubectl(ctx, nil, "get", "namespace", e.Namespace, "-o", "name"); err != nil {
		return fmt.Errorf("test namespace is unavailable: %w", err)
	}
	for _, resource := range []string{"deployments", "services", "pods", "configmaps", "secrets", "leases.coordination.k8s.io"} {
		for _, verb := range []string{"get", "list", "watch", "create", "patch", "update", "delete"} {
			output, err := e.Kubectl(ctx, nil, "auth", "can-i", verb, resource)
			if err != nil || strings.TrimSpace(string(output)) != "yes" {
				return fmt.Errorf("missing namespace permission %s %s", verb, resource)
			}
		}
	}
	if scenario == "feature-ingress" {
		for _, name := range []string{"CLASH_TEST_INGRESS_HOST", "CLASH_TEST_TLS_CERT_FILE", "CLASH_TEST_TLS_KEY_FILE"} {
			if os.Getenv(name) == "" {
				return fmt.Errorf("ingress requires %s", name)
			}
		}
		output, err := e.Kubectl(ctx, nil, "auth", "can-i", "create", "ingresses")
		if err != nil || strings.TrimSpace(string(output)) != "yes" {
			return fmt.Errorf("missing ingress permissions")
		}
	}
	if os.Getenv("CLASH_TEST_FIXTURE_IMAGE") == "" && os.Getenv("CLASH_TEST_IMAGE_REPOSITORY") == "" {
		return fmt.Errorf("set CLASH_TEST_IMAGE_REPOSITORY or an immutable CLASH_TEST_FIXTURE_IMAGE")
	}
	if value := os.Getenv("CLASH_TEST_FIXTURE_IMAGE"); value != "" && !strings.Contains(value, "@sha256:") {
		return fmt.Errorf("CLASH_TEST_FIXTURE_IMAGE must use an immutable digest")
	}
	if _, err := os.Stat(filepath.Join(e.App, "chart/files/console/index.html")); err != nil {
		return fmt.Errorf("build the Console before cluster tests")
	}
	_, err := execute(ctx, filepath.Join(e.App, "tests/browser"), nil, nil, "node", "--input-type=module", "-e", "import {chromium} from '@playwright/test'; import {accessSync} from 'node:fs'; accessSync(process.env.CLASH_TEST_BROWSER_EXECUTABLE || chromium.executablePath());")
	if err != nil {
		return fmt.Errorf("install test browser dependencies and Chromium: %w", err)
	}
	return nil
}
