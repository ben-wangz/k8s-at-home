package environment

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"math/big"
	"net/http"
	"strings"
	"time"
)

func registryS3Pod(name, runID, fullname, credentialsSecret, tlsSecret string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": name, "namespace": Namespace, "labels": fixtureLabels(runID, fullname, name)},
		"spec": map[string]any{
			"automountServiceAccountToken": false, "restartPolicy": "Always",
			"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 65532, "runAsGroup": 65532, "fsGroup": 65532, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
			"containers": []map[string]any{{
				"name": "versitygw", "image": backendS3Image, "imagePullPolicy": "IfNotPresent",
				"args":  []string{"--access-log", "stdout", "--quiet", "--cert", "/certs/tls.crt", "--key", "/certs/tls.key", "--port", ":8080", "--region", "us-east-1", "posix", "/data"},
				"ports": []map[string]any{{"name": "s3", "containerPort": 8080, "protocol": "TCP"}},
				"env": []map[string]any{
					{"name": "HOME", "value": "/tmp"},
					{"name": "ROOT_ACCESS_KEY_ID", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": credentialsSecret, "key": "AWS_ACCESS_KEY_ID"}}},
					{"name": "ROOT_SECRET_ACCESS_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": credentialsSecret, "key": "AWS_SECRET_ACCESS_KEY"}}},
				},
				"readinessProbe":  map[string]any{"tcpSocket": map[string]any{"port": "s3"}, "periodSeconds": 2, "timeoutSeconds": 3, "failureThreshold": 60},
				"livenessProbe":   map[string]any{"tcpSocket": map[string]any{"port": "s3"}, "periodSeconds": 10, "timeoutSeconds": 3, "failureThreshold": 6},
				"resources":       map[string]any{"requests": map[string]string{"cpu": "100m", "memory": "256Mi"}, "limits": map[string]string{"cpu": "1", "memory": "1Gi"}},
				"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
				"volumeMounts":    []map[string]any{{"name": "data", "mountPath": "/data"}, {"name": "tmp", "mountPath": "/tmp"}, {"name": "tls", "mountPath": "/certs", "readOnly": true}},
			}},
			"volumes": []map[string]any{
				{"name": "data", "emptyDir": map[string]any{"sizeLimit": "1Gi"}},
				{"name": "tmp", "emptyDir": map[string]any{}},
				{"name": "tls", "secret": map[string]any{"secretName": tlsSecret, "items": []map[string]any{{"key": "tls.crt", "path": "tls.crt"}, {"key": "tls.key", "path": "tls.key"}}}},
			},
		},
	}
}

func newRegistryS3TLSMaterial(runID, dnsName string) (map[string][]byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caTemplate, err := registryS3CertificateTemplate("registry-hub-test-ca-"+runID, true)
	if err != nil {
		return nil, err
	}
	caTemplate.IsCA = true
	caTemplate.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serverTemplate, err := registryS3CertificateTemplate("registry-hub-test-s3-"+runID, false)
	if err != nil {
		return nil, err
	}
	serverTemplate.DNSNames = []string{dnsName}
	serverTemplate.KeyUsage = x509.KeyUsageDigitalSignature
	serverTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertificate, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverKey)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}),
		"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
	}, nil
}

func registryS3CertificateTemplate(commonName string, isCA bool) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	serial.Add(serial, big.NewInt(1))
	now := time.Now().UTC()
	return &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: commonName},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(365 * 24 * time.Hour),
		BasicConstraintsValid: true, IsCA: isCA,
	}, nil
}

func validateRegistryS3TLSMaterial(data map[string][]byte, dnsName string) error {
	caBlock, _ := pem.Decode(data["ca.crt"])
	serverBlock, _ := pem.Decode(data["tls.crt"])
	keyBlock, _ := pem.Decode(data["tls.key"])
	if caBlock == nil || serverBlock == nil || keyBlock == nil {
		return fmt.Errorf("certificate data is not PEM encoded")
	}
	caCertificate, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return err
	}
	serverCertificate, err := x509.ParseCertificate(serverBlock.Bytes)
	if err != nil {
		return err
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		return err
	}
	if _, err := tls.X509KeyPair(data["tls.crt"], data["tls.key"]); err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCertificate)
	if _, err := serverCertificate.Verify(x509.VerifyOptions{Roots: roots, DNSName: dnsName, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return err
	}
	return nil
}

func newRegistryS3FixtureClient(ctx context.Context, endpoint, dnsName string, caPEM []byte, accessKey, secretKey string) (*s3.Client, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("registry S3 fixture CA is invalid")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: dnsName, MinVersion: tls.VersionTLS12}}
	awsConfig, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithHTTPClient(&http.Client{Transport: transport, Timeout: 20 * time.Second}),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(strings.TrimSuffix(endpoint, "/"))
		options.UsePathStyle = true
	}), nil
}

func digestRegistryS3Fixture(credentialsData, tlsData map[string][]byte) string {
	material := make(map[string]string, len(credentialsData)+len(tlsData))
	for key, value := range credentialsData {
		material["credentials/"+key] = hex.EncodeToString(sha256Bytes(value))
	}
	for key, value := range tlsData {
		material["tls/"+key] = hex.EncodeToString(sha256Bytes(value))
	}
	encoded, _ := json.Marshal(material)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func sha256Bytes(value []byte) []byte {
	digest := sha256.Sum256(value)
	return digest[:]
}
