package defaultrender

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

//go:embed case.json
var metadata embed.FS

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "default-render")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	first, err := render(ctx, environment)
	if err != nil {
		return err
	}
	second, err := render(ctx, environment)
	if err != nil {
		return err
	}
	if string(first) != string(second) {
		return fmt.Errorf("default Helm rendering is not deterministic")
	}
	text := string(first)
	for _, expected := range []string{"kind: Job", "REGISTRY_HUB_INIT_ADMIN_AUTH", "REGISTRY_HUB_INIT_TLS", "REGISTRY_HUB_INIT_HTTP_SECRET"} {
		if !strings.Contains(text, expected) {
			return fmt.Errorf("default Helm rendering is missing %q", expected)
		}
	}
	if err := assertServiceReadinessRouting(text); err != nil {
		return err
	}
	for _, name := range []string{"REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS", "REGISTRY_HUB_LIMIT_CLOSURE_BYTES"} {
		if strings.Contains(text, "- name: "+name) {
			return fmt.Errorf("default Helm rendering overrides application default %s", name)
		}
	}
	for _, sensitive := range []string{"password:", "token:", "tls.key:"} {
		if strings.Contains(text, sensitive) {
			return fmt.Errorf("default Helm rendering contains a sensitive value field")
		}
	}
	if strings.Contains(text, "kind: Secret\n") {
		return fmt.Errorf("default Helm rendering must not pre-create generated Secrets")
	}
	adminExternal, err := renderWithSettings(ctx, environment, "backend.adminAuth.existingSecret=admin-probe")
	if err != nil || !strings.Contains(string(adminExternal), "admin-probe") || !strings.Contains(string(adminExternal), "REGISTRY_HUB_INIT_ADMIN_AUTH") {
		return fmt.Errorf("external management auth with internal initialization did not render")
	}
	internalExternalAdminAuto, err := renderWithSettings(ctx, environment,
		"backend.distributionAuth.existingSecret=distribution-probe",
		"registry.auth.htpasswdSecret=htpasswd-probe",
		"registry.tls.existingSecret=tls-probe",
		"registry.httpSecret.existingSecret=http-probe")
	if err != nil || !strings.Contains(string(internalExternalAdminAuto), "- name: REGISTRY_HUB_INIT_ADMIN_AUTH\n              value: \"true\"") {
		return fmt.Errorf("automatic management auth with external internal Registry settings did not render")
	}
	allExternal, err := renderWithSettings(ctx, environment,
		"backend.adminAuth.existingSecret=admin-probe",
		"backend.distributionAuth.existingSecret=distribution-probe",
		"registry.auth.htpasswdSecret=htpasswd-probe",
		"registry.tls.existingSecret=tls-probe",
		"registry.httpSecret.existingSecret=http-probe")
	if err != nil || strings.Contains(string(allExternal), "kind: Job\n") {
		return fmt.Errorf("fully external authentication and TLS rendered an initialization Job")
	}
	if err := rejectPartialExternalAuth(ctx, environment, "backend.distributionAuth.existingSecret=render-probe"); err != nil {
		return err
	}
	if err := rejectPartialExternalAuth(ctx, environment, "registry.auth.htpasswdSecret=render-probe"); err != nil {
		return err
	}
	pinnedRegistry, err := renderWithSettings(ctx, environment, "registry.image.digest=sha256:"+strings.Repeat("a", 64))
	if err != nil || strings.Count(string(pinnedRegistry), "@sha256:"+strings.Repeat("a", 64)) < 2 {
		return fmt.Errorf("registry image digest pin did not render")
	}
	digest := sha256.Sum256(first)
	return support.WriteResult(environment, map[string]any{
		"renderSHA256":                 hex.EncodeToString(digest[:]),
		"externalCombinations":         "verified",
		"closureLimitOverridesAbsent":  true,
		"adminServiceReadinessRouting": true,
	})
}

func assertServiceReadinessRouting(rendered string) error {
	adminService, ok := renderedServiceWithPort(rendered, "admin")
	if !ok {
		return fmt.Errorf("default Helm rendering is missing the admin Service")
	}
	if !strings.Contains(adminService, "\n  publishNotReadyAddresses: true\n") {
		return fmt.Errorf("admin Service does not publish not-ready backend endpoints")
	}
	clientService, ok := renderedServiceWithPort(rendered, "registry")
	if !ok {
		return fmt.Errorf("default Helm rendering is missing the client Service")
	}
	if strings.Contains(clientService, "\n  publishNotReadyAddresses: true\n") {
		return fmt.Errorf("client Service publishes not-ready backend endpoints")
	}
	return nil
}

func renderedServiceWithPort(rendered, portName string) (string, bool) {
	for _, document := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(document, "kind: Service\n") && strings.Contains(document, "    - name: "+portName+"\n") {
			return document, true
		}
	}
	return "", false
}

func render(ctx context.Context, environment testkit.CaseContext) ([]byte, error) {
	return renderWithSettings(ctx, environment)
}

func renderWithSettings(ctx context.Context, environment testkit.CaseContext, settings ...string) ([]byte, error) {
	arguments := []string{"template", "rh-default-" + environment.RunID, environment.ChartDir,
		"--namespace", environment.Namespace, "--values", environment.BaseValues}
	for _, setting := range settings {
		arguments = append(arguments, "--set-string", setting)
	}
	result, err := testkit.Command(ctx, "helm", arguments...)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}

func rejectPartialExternalAuth(ctx context.Context, environment testkit.CaseContext, setting string) error {
	_, err := testkit.Command(ctx, "helm", "template", "rh-default-"+environment.RunID, environment.ChartDir,
		"--namespace", environment.Namespace, "--values", environment.BaseValues, "--set-string", setting)
	if err == nil || !strings.Contains(err.Error(), "must both be empty or both reference external Secrets") {
		return fmt.Errorf("Helm did not reject the incomplete external authentication configuration")
	}
	return nil
}
