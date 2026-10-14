package initializationsecrets

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"golang.org/x/crypto/bcrypt"
)

//go:embed case.json
var metadata embed.FS

type internalCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

func New() (testkit.Case, error) {
	definition, raw, err := testkit.LoadDefinition(metadata, "initialization-secrets")
	if err != nil {
		return testkit.Case{}, err
	}
	return testkit.Case{Definition: definition, Raw: raw, Run: Run}, nil
}

func Run(ctx context.Context, environment testkit.CaseContext) error {
	secretName := environment.Fullname + "-registry-internal"
	secret, data, err := support.GetSecret(ctx, environment.Namespace, secretName)
	if err != nil {
		return err
	}
	expectedKeys := []string{"ca.crt", "config.json", "htpasswd", "http-secret", "tls.crt", "tls.key"}
	if strings.Join(support.SortedKeys(data), "\x00") != strings.Join(expectedKeys, "\x00") {
		return fmt.Errorf("internal Registry Secret has an unexpected key set")
	}
	annotations := testkit.Map(secret.Metadata["annotations"])
	if secret.Labels["registry-hub.io/instance"] != environment.Fullname ||
		secret.Labels["registry-hub.io/managed-by"] != "internal-initializer" ||
		testkit.String(annotations["registry-hub.io/internal-scope"]) == "" {
		return fmt.Errorf("internal Registry Secret ownership metadata is incomplete")
	}
	generated, err := support.GetGeneratedObjects(ctx, environment.Namespace, environment.Fullname)
	if err != nil {
		return err
	}
	record, err := support.FindObject(generated, "ConfigMap", "internal-initializer")
	if err != nil {
		return err
	}
	state, err := support.DecodeRecord(record)
	if err != nil {
		return err
	}
	if state.Protocol != "v1" || state.Instance != environment.Fullname || state.SecretUID != secret.UID {
		return fmt.Errorf("internal initialization record does not match the Secret UID")
	}
	if err := verifyAuthentication(data["config.json"], data["htpasswd"]); err != nil {
		return err
	}
	if err := verifyTLS(data["ca.crt"], data["tls.crt"], data["tls.key"], environment); err != nil {
		return err
	}
	if len(data["http-secret"]) != 64 {
		return fmt.Errorf("Distribution HTTP secret has an invalid length")
	}
	if _, err := hex.DecodeString(string(data["http-secret"])); err != nil {
		return fmt.Errorf("Distribution HTTP secret is not hexadecimal")
	}
	scope := testkit.String(annotations["registry-hub.io/internal-scope"])
	return support.WriteResult(environment, map[string]any{
		"secretUID":   secret.UID,
		"keySHA256":   support.SecretHashes(data),
		"recordUID":   record.UID,
		"scopeSHA256": sha256String(scope),
	})
}

func verifyAuthentication(authJSON, htpasswd []byte) error {
	var credentials internalCredential
	if err := json.Unmarshal(authJSON, &credentials); err != nil {
		return fmt.Errorf("internal Registry auth JSON is invalid")
	}
	if credentials.Username != "registry-hub-backend" || credentials.Password == "" || credentials.Token != "" {
		return fmt.Errorf("internal Registry auth credential is invalid")
	}
	matched := false
	for _, line := range strings.Split(string(htpasswd), "\n") {
		username, hash, found := strings.Cut(line, ":")
		if found && username == credentials.Username && bcrypt.CompareHashAndPassword([]byte(hash), []byte(credentials.Password)) == nil {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("internal Registry JSON and htpasswd credentials do not match")
	}
	return nil
}

func verifyTLS(caPEM, certPEM, keyPEM []byte, environment testkit.CaseContext) error {
	caBlock, _ := pem.Decode(caPEM)
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if caBlock == nil || certBlock == nil || keyBlock == nil {
		return fmt.Errorf("internal TLS Secret contains invalid PEM data")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !caCert.IsCA {
		return fmt.Errorf("internal TLS CA certificate is invalid")
	}
	leaf, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return fmt.Errorf("internal TLS server certificate is invalid")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return fmt.Errorf("internal TLS private key is invalid")
	}
	if err := verifyKeyPair(leaf, privateKey); err != nil {
		return err
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) || !leaf.NotAfter.After(now.Add(24*time.Hour)) {
		return fmt.Errorf("internal TLS certificate validity is insufficient")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	serviceName := environment.Fullname + "-registry"
	for _, dnsName := range []string{serviceName, serviceName + "." + environment.Namespace + ".svc", serviceName + "." + environment.Namespace + ".svc.cluster.local"} {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: dnsName, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("internal TLS certificate SAN verification failed for %s", dnsName)
		}
	}
	return nil
}

func verifyKeyPair(leaf *x509.Certificate, key *rsa.PrivateKey) error {
	certPublic, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil {
		return err
	}
	keyPublic, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return err
	}
	if !bytes.Equal(certPublic, keyPublic) {
		return fmt.Errorf("internal TLS certificate and private key do not match")
	}
	return nil
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
