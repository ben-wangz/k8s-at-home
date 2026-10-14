package environment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type authCredential struct {
	Username    string   `json:"username"`
	Password    string   `json:"password,omitempty"`
	Token       string   `json:"token,omitempty"`
	Permissions []string `json:"permissions"`
}

type authConfiguration struct {
	Basic  []authCredential `json:"basic"`
	Bearer []authCredential `json:"bearer"`
}

func (environment Environment) PrepareClientAuthSecret(ctx context.Context, runID, fullname, expectedDigest string) (string, string, error) {
	release := strings.TrimSuffix(fullname, "-registry-hub")
	if release == fullname || release == "" {
		return "", "", fmt.Errorf("client auth fixture has an invalid release identity")
	}
	name := release + "-external-client-auth"
	object, err := testkit.KubectlGet(ctx, Namespace, "secret", name)
	if err != nil && !testkit.IsNotFound(err) {
		return "", "", err
	}
	if testkit.IsNotFound(err) && expectedDigest != "" {
		return "", "", fmt.Errorf("client auth fixture Secret %s is missing for a preserved run", name)
	}
	if testkit.IsNotFound(err) {
		configuration, configBytes, err := newAuthConfiguration()
		if err != nil {
			return "", "", err
		}
		if err := validateAuthConfiguration(configuration); err != nil {
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
		return "", "", fmt.Errorf("client auth fixture Secret %s has unexpected ownership or type", name)
	}
	decoded, err := testkit.DecodeSecretData(testkit.Object{Kind: "Secret", Name: name, Data: testkit.Map(object["data"])})
	if err != nil {
		return "", "", err
	}
	if len(decoded) != 1 || len(decoded["config.json"]) == 0 {
		return "", "", fmt.Errorf("client auth fixture Secret %s has unexpected keys", name)
	}
	var configuration authConfiguration
	if err := json.Unmarshal(decoded["config.json"], &configuration); err != nil {
		return "", "", fmt.Errorf("client auth fixture Secret %s is invalid", name)
	}
	if err := validateAuthConfiguration(configuration); err != nil {
		return "", "", fmt.Errorf("client auth fixture Secret %s is invalid: %w", name, err)
	}
	digest := sha256.Sum256(decoded["config.json"])
	return name, hex.EncodeToString(digest[:]), nil
}

func newAuthConfiguration() (authConfiguration, []byte, error) {
	readerPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	catalogPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	writerPassword, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	bearerToken, err := randomCredential()
	if err != nil {
		return authConfiguration{}, nil, err
	}
	configuration := authConfiguration{
		Basic: []authCredential{
			{Username: "reader", Password: readerPassword, Permissions: []string{"pull"}},
			{Username: "catalog-reader", Password: catalogPassword, Permissions: []string{"catalog"}},
			{Username: "writer", Password: writerPassword, Permissions: []string{"pull", "push", "delete"}},
		},
		Bearer: []authCredential{{Username: "bearer-reader", Token: bearerToken, Permissions: []string{"pull"}}},
	}
	encoded, err := json.Marshal(configuration)
	return configuration, encoded, err
}

func validateAuthConfiguration(configuration authConfiguration) error {
	if len(configuration.Basic) != 3 || len(configuration.Bearer) != 1 {
		return fmt.Errorf("expected three Basic credentials and one Bearer credential")
	}
	if configuration.Basic[0].Username != "reader" || len(configuration.Basic[0].Password) < 32 || strings.Join(configuration.Basic[0].Permissions, " ") != "pull" {
		return fmt.Errorf("pull credential does not match the test contract")
	}
	if configuration.Basic[1].Username != "catalog-reader" || len(configuration.Basic[1].Password) < 32 || strings.Join(configuration.Basic[1].Permissions, " ") != "catalog" {
		return fmt.Errorf("catalog credential does not match the test contract")
	}
	if configuration.Basic[2].Username != "writer" || len(configuration.Basic[2].Password) < 32 || strings.Join(configuration.Basic[2].Permissions, " ") != "pull push delete" {
		return fmt.Errorf("writer credential does not match the test contract")
	}
	if configuration.Bearer[0].Username != "bearer-reader" || len(configuration.Bearer[0].Token) < 32 || strings.Join(configuration.Bearer[0].Permissions, " ") != "pull" {
		return fmt.Errorf("Bearer credential does not match the test contract")
	}
	return nil
}

func randomCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
