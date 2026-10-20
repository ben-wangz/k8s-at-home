package environment

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

func (e *Environment) Fixtures(ctx context.Context, directory string, record *Record) error {
	registration := e.labeled(record, "ConfigMap", record.Release+"-run")
	registration.Data = map[string]string{"id": record.ID, "scenario": record.Scenario, "sourceSHA256": record.SourceSHA}
	if err := e.apply(ctx, registration); err != nil {
		return fmt.Errorf("cannot register a new run: %w", err)
	}
	name := record.Release + "-fixture"
	labels := map[string]string{RunLabel: record.ID, "clash-console.test/component": "fixture"}
	metadata := map[string]any{"name": name, "namespace": e.Namespace, "labels": labels}
	service := map[string]any{"apiVersion": "v1", "kind": "Service", "metadata": metadata, "spec": map[string]any{"selector": labels, "ports": []any{map[string]any{"name": "http", "port": 8080, "targetPort": 8080}}}}
	if err := e.apply(ctx, service); err != nil {
		return err
	}
	host := name + "." + e.Namespace + ".svc.cluster.local"
	deployment := map[string]any{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": metadata, "spec": map[string]any{
		"replicas": 1, "selector": map[string]any{"matchLabels": labels}, "template": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{
			"containers": []any{map[string]any{"name": "fixture", "image": record.FixtureImage,
				"env":             []any{map[string]string{"name": "CLASH_FIXTURE_HOST", "value": host}},
				"ports":           []any{map[string]any{"containerPort": 8080}},
				"readinessProbe":  map[string]any{"httpGet": map[string]any{"path": "/health", "port": 8080}, "periodSeconds": 2},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "20m", "memory": "24Mi"}, "limits": map[string]string{"cpu": "200m", "memory": "64Mi"}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			}},
		}},
	}}
	if err := e.apply(ctx, deployment); err != nil {
		return err
	}
	secret := e.labeled(record, "Secret", record.Release+"-subscription")
	secret.Data = map[string]string{"subscription-url": base64.StdEncoding.EncodeToString([]byte("http://" + host + ":8080/subscription"))}
	if err := e.apply(ctx, secret); err != nil {
		return err
	}
	if record.Scenario == "feature-controller-auth" || record.Scenario == "feature-ingress" {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		credential := hex.EncodeToString(value)
		if err := os.WriteFile(filepath.Join(directory, "controller-secret"), []byte(credential), 0600); err != nil {
			return err
		}
		auth := e.labeled(record, "Secret", record.Release+"-controller")
		auth.Data = map[string]string{"secret": base64.StdEncoding.EncodeToString([]byte(credential))}
		if err := e.apply(ctx, auth); err != nil {
			return err
		}
	}
	if record.Scenario == "feature-ingress" {
		tls := e.labeled(record, "Secret", record.Release+"-tls")
		tls.Type = "kubernetes.io/tls"
		tls.Data = make(map[string]string)
		for key, variable := range map[string]string{"tls.crt": "CLASH_TEST_TLS_CERT_FILE", "tls.key": "CLASH_TEST_TLS_KEY_FILE"} {
			value, err := os.ReadFile(os.Getenv(variable))
			if err != nil {
				return err
			}
			tls.Data[key] = base64.StdEncoding.EncodeToString(value)
		}
		if err := e.apply(ctx, tls); err != nil {
			return err
		}
	}
	_, err := e.Kubectl(ctx, nil, "rollout", "status", "deployment/"+name, "--timeout=180s")
	return err
}
