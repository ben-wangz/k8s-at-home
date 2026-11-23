package environment

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func (environment Environment) PrepareHistoryAdminSecret(ctx context.Context, runID, fullname, expectedDigest string) (string, string, error) {
	release := strings.TrimSuffix(fullname, "-registry-hub")
	if release == fullname || release == "" {
		return "", "", fmt.Errorf("history admin fixture has an invalid release identity")
	}
	name := release + "-external-history-admin"
	object, err := testkit.KubectlGet(ctx, Namespace, "secret", name)
	if err != nil && !testkit.IsNotFound(err) {
		return "", "", err
	}
	if testkit.IsNotFound(err) && expectedDigest != "" {
		return "", "", fmt.Errorf("history admin fixture Secret %s is missing for a preserved run", name)
	}
	if testkit.IsNotFound(err) {
		configuration, configBytes, err := newHistoryAdminConfiguration()
		if err != nil {
			return "", "", err
		}
		if err := validateHistoryAdminConfiguration(configuration); err != nil {
			return "", "", err
		}
		payload := map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"type":       "Opaque",
			"immutable":  true,
			"metadata": map[string]any{
				"name":      name,
				"namespace": Namespace,
				"labels": map[string]string{
					"registry-hub.io/test-id":    runID,
					"registry-hub.io/instance":   fullname,
					"registry-hub.io/managed-by": "registry-hub-test-fixture",
				},
			},
			"data": map[string]string{"config.json": base64.StdEncoding.EncodeToString(configBytes)},
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return "", "", err
		}
		if _, createErr := testkit.CommandInput(ctx, encoded, "kubectl", "create", "-f", "-"); createErr != nil {
			object, err = testkit.KubectlGet(ctx, Namespace, "secret", name)
			if err != nil {
				return "", "", createErr
			}
		} else {
			object, err = testkit.KubectlGet(ctx, Namespace, "secret", name)
			if err != nil {
				return "", "", err
			}
		}
	}
	metadata := testkit.Map(object["metadata"])
	labels := testkit.Map(metadata["labels"])
	if testkit.String(metadata["name"]) != name || testkit.String(metadata["namespace"]) != Namespace ||
		testkit.String(metadata["uid"]) == "" || object["type"] != "Opaque" || object["immutable"] != true ||
		testkit.String(labels["registry-hub.io/test-id"]) != runID ||
		testkit.String(labels["registry-hub.io/instance"]) != fullname ||
		testkit.String(labels["registry-hub.io/managed-by"]) != "registry-hub-test-fixture" {
		return "", "", fmt.Errorf("history admin fixture Secret %s has unexpected ownership or type", name)
	}
	decoded, err := testkit.DecodeSecretData(testkit.Object{Kind: "Secret", Name: name, Data: testkit.Map(object["data"])})
	if err != nil {
		return "", "", err
	}
	if len(decoded) != 1 || len(decoded["config.json"]) == 0 {
		return "", "", fmt.Errorf("history admin fixture Secret %s has unexpected keys", name)
	}
	var configuration authConfiguration
	if err := json.Unmarshal(decoded["config.json"], &configuration); err != nil {
		return "", "", fmt.Errorf("history admin fixture Secret %s is invalid", name)
	}
	if err := validateHistoryAdminConfiguration(configuration); err != nil {
		return "", "", fmt.Errorf("history admin fixture Secret %s is invalid: %w", name, err)
	}
	digest := sha256.Sum256(decoded["config.json"])
	return name, hex.EncodeToString(digest[:]), nil
}

func newHistoryAdminConfiguration() (authConfiguration, []byte, error) {
	operatorPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	observerPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	taskAdminPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	configuration := authConfiguration{Basic: []authCredential{
		{Username: "history-operator", Password: operatorPassword, Permissions: []string{"history-cleanup", "observe"}},
		{Username: "history-observer", Password: observerPassword, Permissions: []string{"observe"}},
		{Username: "task-admin", Password: taskAdminPassword, Permissions: []string{"task-admin"}},
	}}
	encoded, err := json.Marshal(configuration)
	return configuration, encoded, err
}

func validateHistoryAdminConfiguration(configuration authConfiguration) error {
	if len(configuration.Basic) != 3 || len(configuration.Bearer) != 0 {
		return fmt.Errorf("expected three Basic management credentials and no Bearer credentials")
	}
	expected := map[string]string{
		"history-operator": "history-cleanup observe",
		"history-observer": "observe",
		"task-admin":       "task-admin",
	}
	for _, credential := range configuration.Basic {
		permissions, exists := expected[credential.Username]
		if !exists || len(credential.Password) < 32 || credential.Token != "" || strings.Join(credential.Permissions, " ") != permissions {
			return fmt.Errorf("management credential does not match the history cleanup test contract")
		}
		delete(expected, credential.Username)
	}
	if len(expected) != 0 {
		return fmt.Errorf("history cleanup test credentials are incomplete")
	}
	return nil
}
