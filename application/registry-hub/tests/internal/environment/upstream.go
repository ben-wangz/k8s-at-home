package environment

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"math/big"
	"strings"
	"time"
)

const upstreamConfigSecretSuffix = "-external-upstream-auth"

type upstreamCredential struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type upstreamTransportRule struct {
	AllowHTTP bool     `json:"allowHTTP"`
	CAFile    string   `json:"caFile"`
	Ports     []uint16 `json:"ports"`
}

type upstreamProxyConfig struct {
	Default     string            `json:"default"`
	Authorities map[string]string `json:"authorities"`
}

func (environment Environment) PrepareUpstreamConfigSecret(ctx context.Context, runID, fullname, expectedDigest string) (string, string, error) {
	release := strings.TrimSuffix(fullname, "-registry-hub")
	if release == fullname || release == "" {
		return "", "", fmt.Errorf("upstream config fixture has an invalid release identity")
	}
	name := release + upstreamConfigSecretSuffix
	object, err := testkit.KubectlGet(ctx, Namespace, "secret", name)
	if err != nil && !testkit.IsNotFound(err) {
		return "", "", err
	}
	if testkit.IsNotFound(err) && expectedDigest != "" {
		return "", "", fmt.Errorf("upstream config fixture Secret %s is missing for a preserved run", name)
	}
	if testkit.IsNotFound(err) {
		data, err := newUpstreamConfigData(release)
		if err != nil {
			return "", "", err
		}
		if err := validateUpstreamConfigData(data, release); err != nil {
			return "", "", err
		}
		encodedData := make(map[string]string, len(data))
		for key, value := range data {
			encodedData[key] = base64.StdEncoding.EncodeToString(value)
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
			"data": encodedData,
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
		return "", "", fmt.Errorf("upstream config fixture Secret %s has unexpected ownership or type", name)
	}
	decoded, err := testkit.DecodeSecretData(testkit.Object{Kind: "Secret", Name: name, Data: testkit.Map(object["data"])})
	if err != nil {
		return "", "", err
	}
	if err := validateUpstreamConfigData(decoded, release); err != nil {
		return "", "", fmt.Errorf("upstream config fixture Secret %s is invalid: %w", name, err)
	}
	digest := digestUpstreamConfigData(decoded)
	if expectedDigest != "" && digest != expectedDigest {
		return "", "", fmt.Errorf("upstream config fixture Secret %s changed for a preserved run", name)
	}
	return name, digest, nil
}

func newUpstreamConfigData(release string) (map[string][]byte, error) {
	password, err := randomCredential()
	if err != nil {
		return nil, err
	}
	token, err := randomCredential()
	if err != nil {
		return nil, err
	}
	certificate, privateKey, err := newUpstreamTLSMaterial()
	if err != nil {
		return nil, err
	}
	caFile := "/etc/registry-hub/upstream-tls/ca.crt"
	values := map[string]any{
		"REGISTRY_HUB_UPSTREAM_AUTH_CONFIG": map[string]upstreamCredential{
			"docker.io": {Type: "basic", Username: "registry-hub-test", Password: password},
			"ghcr.io":   {Type: "bearer", Token: token},
		},
		"REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG": map[string]upstreamTransportRule{
			"docker.io":                       {AllowHTTP: true, Ports: []uint16{80}},
			"github.com":                      {AllowHTTP: true, CAFile: caFile, Ports: []uint16{80, 443}},
			"ghcr.io":                         {AllowHTTP: true, Ports: []uint16{80}},
			"tls.registry-hub.test":           {CAFile: caFile, Ports: []uint16{443}},
			"tls-untrusted.registry-hub.test": {Ports: []uint16{443}},
		},
		"REGISTRY_HUB_PROXY_CONFIG": upstreamProxyConfig{
			Authorities: map[string]string{
				"docker.io":                       upstreamProxyURL(release),
				"github.com":                      upstreamProxyURL(release),
				"ghcr.io":                         upstreamProxyURL(release),
				"tls.registry-hub.test":           upstreamProxyURL(release),
				"tls-untrusted.registry-hub.test": upstreamProxyURL(release),
			},
		},
	}
	result := make(map[string][]byte, len(values)+3)
	for key, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result[key] = encoded
	}
	result["UPSTREAM_TLS_CERT"] = certificate
	result["UPSTREAM_TLS_KEY"] = privateKey
	result["UPSTREAM_TLS_CA_CERT"] = certificate
	return result, nil
}

func validateUpstreamConfigData(data map[string][]byte, release string) error {
	if len(data) != 6 {
		return fmt.Errorf("expected three upstream configuration keys and three TLS fixture keys")
	}
	if err := validateUpstreamTLSMaterial(data["UPSTREAM_TLS_CERT"], data["UPSTREAM_TLS_KEY"], data["UPSTREAM_TLS_CA_CERT"]); err != nil {
		return fmt.Errorf("upstream TLS fixture material is invalid: %w", err)
	}
	var auth map[string]upstreamCredential
	if err := json.Unmarshal(data["REGISTRY_HUB_UPSTREAM_AUTH_CONFIG"], &auth); err != nil {
		return fmt.Errorf("upstream auth configuration is invalid")
	}
	credential, ok := auth["docker.io"]
	bearer, bearerOK := auth["ghcr.io"]
	if len(auth) != 2 || !ok || credential.Type != "basic" || credential.Username != "registry-hub-test" || len(credential.Password) < 32 ||
		!bearerOK || bearer.Type != "bearer" || len(bearer.Token) < 32 {
		return fmt.Errorf("upstream auth configuration does not match the fixture contract")
	}
	var transports map[string]upstreamTransportRule
	if err := json.Unmarshal(data["REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG"], &transports); err != nil {
		return fmt.Errorf("upstream transport configuration is invalid")
	}
	caFile := "/etc/registry-hub/upstream-tls/ca.crt"
	if len(transports) != 5 || !validHTTPRule(transports["docker.io"]) || !validHTTPAndTLSRule(transports["github.com"], caFile) || !validHTTPRule(transports["ghcr.io"]) ||
		!validTLSRule(transports["tls.registry-hub.test"], caFile) || !validTLSRule(transports["tls-untrusted.registry-hub.test"], "") {
		return fmt.Errorf("upstream transport configuration does not match the fixture contract")
	}
	var proxy upstreamProxyConfig
	if err := json.Unmarshal(data["REGISTRY_HUB_PROXY_CONFIG"], &proxy); err != nil {
		return fmt.Errorf("upstream proxy configuration is invalid")
	}
	proxyURL := upstreamProxyURL(release)
	if proxy.Default != "" || len(proxy.Authorities) != 5 || proxy.Authorities["docker.io"] != proxyURL ||
		proxy.Authorities["github.com"] != proxyURL || proxy.Authorities["ghcr.io"] != proxyURL ||
		proxy.Authorities["tls.registry-hub.test"] != proxyURL || proxy.Authorities["tls-untrusted.registry-hub.test"] != proxyURL {
		return fmt.Errorf("upstream proxy configuration does not match the fixture contract")
	}
	return nil
}

func newUpstreamTLSMaterial() ([]byte, []byte, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "tls.registry-hub.test"},
		DNSNames:              []string{"tls.registry-hub.test", "github.com"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certificate, key, nil
}
