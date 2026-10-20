package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ben-wangz/clash-tests/internal/testkit"
	"gopkg.in/yaml.v3"
)

func renderChecks(ctx context.Context, app, directory string, definition testkit.Definition) ([]testkit.Result, error) {
	var evidence []testkit.Evidence
	variants := []struct {
		name                string
		args                []string
		required, forbidden []string
	}{
		{"default", nil, []string{"external-ui: \"/root/.config/mihomo/ui\"", "index.html:", "mountPath: \"/root/.config/mihomo/ui\"", "subscription", "MATCH,PROXY"}, []string{"secret: |2-"}},
		{"disabled", []string{"--set", "console.enabled=false"}, []string{"external-controller:"}, []string{"external-ui:", "index.html:"}},
		{"auth", []string{"--set", "controller.existingSecret=auth-fixture"}, []string{"__CONTROLLER_SECRET__", "secretName: \"auth-fixture\"", "mountPath: /input/controller-secret"}, nil},
		{"external-assets", []string{"--set", "console.existingConfigMap=external-console"}, []string{"name: external-console", "external-ui:"}, []string{"index.html:"}},
		{"persistence", []string{"--set", "persistence.enabled=true"}, []string{"kind: PersistentVolumeClaim", "persistentVolumeClaim:", "store-selected: true"}, nil},
		{"ingress", []string{"--set", "ingress.controller.enabled=true", "--set", "ingress.controller.tls=true", "--set", "ingress.controller.secretName=test-tls"}, []string{"kind: Ingress", "secretName: \"test-tls\"", "path: /"}, nil},
	}
	for _, variant := range variants {
		args := append([]string{"template", "clash-test-render", filepath.Join(app, "chart"), "--namespace", "develop", "--set", "subscription.existingSecret=subscription-fixture"}, variant.args...)
		output, err := testkit.Command(ctx, app, nil, "helm", args...)
		if err != nil {
			return nil, fmt.Errorf("%s chart rendering failed: %s", variant.name, output)
		}
		decoder := yaml.NewDecoder(strings.NewReader(string(output)))
		for {
			var document map[string]any
			err := decoder.Decode(&document)
			if err != nil {
				if err != io.EOF {
					return nil, err
				}
				break
			}
		}
		for _, required := range variant.required {
			if !strings.Contains(string(output), required) {
				return nil, fmt.Errorf("%s rendering is missing %s", variant.name, required)
			}
		}
		for _, forbidden := range variant.forbidden {
			if strings.Contains(string(output), forbidden) {
				return nil, fmt.Errorf("%s rendering includes unexpected content %s", variant.name, forbidden)
			}
		}
		path := filepath.Join(directory, variant.name+".yaml")
		if err := os.WriteFile(path, output, 0600); err != nil {
			return nil, err
		}
		evidence = append(evidence, testkit.Evidence{ID: definition.Checks[0].ExpectedEvidence[0].ID, Path: path})
	}
	output, err := testkit.Command(ctx, app, nil, "helm", "template", "bad-path", filepath.Join(app, "chart"), "--set", "subscription.existingSecret=fixture", "--set", "ingress.controller.enabled=true", "--set", "ingress.controller.path=/wrong")
	if err == nil || !strings.Contains(string(output), "requires the controller ingress path") {
		return nil, fmt.Errorf("invalid Console ingress path was accepted")
	}
	output, err = testkit.Command(ctx, app, nil, "helm", "template", "bad-ui-path", filepath.Join(app, "chart"), "--set", "subscription.existingSecret=fixture", "--set", "console.mountPath=/opt/clash-console")
	if err == nil || !strings.Contains(string(output), "must be below config.dir") {
		return nil, fmt.Errorf("Console path outside Mihomo config directory was accepted")
	}
	output, err = testkit.Command(ctx, app, nil, "helm", "template", "traversal-ui-path", filepath.Join(app, "chart"), "--set", "subscription.existingSecret=fixture", "--set", "console.mountPath=/root/.config/mihomo/../outside")
	if err == nil || !strings.Contains(string(output), "must be below config.dir") {
		return nil, fmt.Errorf("Console path traversal was accepted")
	}
	return []testkit.Result{{ID: definition.Checks[0].ID, Status: "passed", Evidence: evidence}}, nil
}
