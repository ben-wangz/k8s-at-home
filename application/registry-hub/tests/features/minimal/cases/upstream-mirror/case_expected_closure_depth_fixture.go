package upstreammirror

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"math/big"
	"strconv"
	"time"
)

func expectedClosureDepthFixture() (nestedExpectedObject, []string, error) {
	digests := make([]string, 66)
	var child nestedExpectedObject
	for depth := len(digests) - 1; depth >= 0; depth-- {
		manifests := []any{}
		if len(child.body) > 0 {
			manifests = append(manifests, map[string]any{
				"mediaType": child.mediaType,
				"size":      len(child.body),
				"digest":    support.Digest(child.body),
			})
		}
		body, err := json.Marshal(map[string]any{
			"schemaVersion": 2,
			"mediaType":     support.OCIIndexMediaType,
			"annotations":   map[string]string{"org.registry-hub.fixture.depth": strconv.Itoa(depth)},
			"manifests":     manifests,
		})
		if err != nil {
			return nestedExpectedObject{}, nil, err
		}
		child = nestedExpectedObject{body: body, mediaType: support.OCIIndexMediaType}
		digests[depth] = support.Digest(body)
	}
	return child, digests, nil
}

func prepareFixtureSecret(ctx context.Context, environment testkit.CaseContext) (testkit.Object, map[string][]byte, error) {
	name := environment.Release + "-external-upstream-auth"
	secret, data, err := support.GetSecret(ctx, environment.Namespace, name)
	if err != nil && !testkit.IsNotFound(err) {
		return testkit.Object{}, nil, err
	}
	if testkit.IsNotFound(err) {
		data, err = newFixtureSecretData()
		if err != nil {
			return testkit.Object{}, nil, err
		}
		encodedData := make(map[string]string, len(data))
		for key, value := range data {
			encodedData[key] = base64.StdEncoding.EncodeToString(value)
		}
		payload := map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "immutable": true,
			"metadata": map[string]any{
				"name": name, "namespace": environment.Namespace,
				"labels": fixtureLabels(environment, environment.Release+"-external-upstream-auth"),
			},
			"data": encodedData,
		}
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return testkit.Object{}, nil, marshalErr
		}
		if _, createErr := testkit.CommandInput(ctx, encoded, "kubectl", "create", "-f", "-"); createErr != nil {
			if _, _, getErr := support.GetSecret(ctx, environment.Namespace, name); getErr != nil {
				return testkit.Object{}, nil, createErr
			}
		}
		secret, data, err = support.GetSecret(ctx, environment.Namespace, name)
		if err != nil {
			return testkit.Object{}, nil, err
		}
	}
	if secret.Name != name || secret.UID == "" || secret.Labels["registry-hub.io/test-id"] != environment.RunID ||
		secret.Labels["registry-hub.io/instance"] != environment.Fullname || secret.Labels["registry-hub.io/managed-by"] != "registry-hub-test-fixture" || len(data) != 4 {
		return testkit.Object{}, nil, fmt.Errorf("upstream fixture Secret ownership or shape is invalid")
	}
	var auth map[string]upstreamCredential
	if err := json.Unmarshal(data["REGISTRY_HUB_UPSTREAM_AUTH_CONFIG"], &auth); err != nil {
		return testkit.Object{}, nil, fmt.Errorf("upstream fixture authentication data is invalid")
	}
	if len(auth) != 2 || auth["docker.io"].Type != "basic" || auth["docker.io"].Username == "" || auth["docker.io"].Password == "" ||
		auth["ghcr.io"].Type != "bearer" || auth["ghcr.io"].Token == "" || string(data["UPSTREAM_TLS_CERT"]) != string(data["UPSTREAM_TLS_CA_CERT"]) {
		return testkit.Object{}, nil, fmt.Errorf("upstream fixture Secret values are invalid")
	}
	if _, err := tls.X509KeyPair(data["UPSTREAM_TLS_CERT"], data["UPSTREAM_TLS_KEY"]); err != nil {
		return testkit.Object{}, nil, fmt.Errorf("upstream fixture TLS key pair is invalid")
	}
	return secret, data, nil
}

func newFixtureSecretData() (map[string][]byte, error) {
	password, err := randomCredential()
	if err != nil {
		return nil, err
	}
	token, err := randomCredential()
	if err != nil {
		return nil, err
	}
	certificate, privateKey, err := fixtureTLSMaterial()
	if err != nil {
		return nil, err
	}
	auth, err := json.Marshal(map[string]upstreamCredential{
		"docker.io": {Type: "basic", Username: "registry-hub-test", Password: password},
		"ghcr.io":   {Type: "bearer", Token: token},
	})
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"REGISTRY_HUB_UPSTREAM_AUTH_CONFIG": auth,
		"UPSTREAM_TLS_CERT":                 certificate,
		"UPSTREAM_TLS_KEY":                  privateKey,
		"UPSTREAM_TLS_CA_CERT":              certificate,
	}, nil
}

func fixtureTLSMaterial() ([]byte, []byte, error) {
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
		Subject:               pkix.Name{CommonName: "github.com"},
		DNSNames:              []string{"github.com", "quay.io"},
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

func randomCredential() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func configureBackendProxyEnvironment(ctx context.Context, environment testkit.CaseContext) error {
	deployment, err := testkit.KubectlGet(ctx, environment.Namespace, "deployment", environment.Fullname)
	if err != nil {
		return err
	}
	spec := testkit.Map(deployment["spec"])
	template := testkit.Map(spec["template"])
	podSpec := testkit.Map(template["spec"])
	if err := ensureGithubAlias(podSpec); err != nil {
		return err
	}
	configMapName := environment.Release + "-external-upstream-proxy"
	if err := ensureCABundleVolume(podSpec, configMapName); err != nil {
		return err
	}
	containerFound := false
	proxyURL := "http://" + configMapName + "." + environment.Namespace + ".svc.cluster.local:8080"
	for _, rawContainer := range testkit.Slice(podSpec["containers"]) {
		container := testkit.Map(rawContainer)
		if testkit.String(container["name"]) != "backend" {
			continue
		}
		containerFound = true
		env := testkit.Slice(container["env"])
		for name, value := range map[string]string{
			"HTTP_PROXY": proxyURL, "HTTPS_PROXY": proxyURL, "http_proxy": proxyURL, "https_proxy": proxyURL,
			"NO_PROXY": "localhost,127.0.0.1,.svc,.cluster.local", "no_proxy": "localhost,127.0.0.1,.svc,.cluster.local",
			"SSL_CERT_FILE": "/etc/registry-hub/test-upstream-ca/ca-bundle.crt",
		} {
			env = setEnvValue(env, name, value)
		}
		container["env"] = env
		container["volumeMounts"] = ensureCAMount(testkit.Slice(container["volumeMounts"]))
	}
	if !containerFound {
		return fmt.Errorf("backend container is missing from its Deployment")
	}
	template["spec"] = podSpec
	spec["template"] = template
	deployment["spec"] = spec
	encoded, err := json.Marshal(deployment)
	if err != nil {
		return err
	}
	if _, err := testkit.CommandInput(ctx, encoded, "kubectl", "replace", "-f", "-"); err != nil {
		return fmt.Errorf("configure minimal upstream fixture environment: %w", err)
	}
	if _, err := testkit.Command(ctx, "kubectl", "-n", environment.Namespace, "rollout", "status", "deployment/"+environment.Fullname, "--timeout=180s"); err != nil {
		return fmt.Errorf("backend did not become ready with test upstream proxy: %w", err)
	}
	return upstreamauthtransport.WaitBackendServiceEndpoint(ctx, environment)
}

func ensureGithubAlias(podSpec map[string]any) error {
	aliases := testkit.Slice(podSpec["hostAliases"])
	for _, raw := range aliases {
		alias := testkit.Map(raw)
		for _, rawHost := range testkit.Slice(alias["hostnames"]) {
			if testkit.String(rawHost) == "github.com" {
				if testkit.String(alias["ip"]) != publicAuthorityIP {
					return fmt.Errorf("github.com already has a different controlled-test address")
				}
				return nil
			}
		}
	}
	podSpec["hostAliases"] = append(aliases, map[string]any{"ip": publicAuthorityIP, "hostnames": []string{"github.com"}})
	return nil
}
