package initialize

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

func validateTLS(caPEM, certificatePEM, privateKeyPEM []byte, options Options) error {
	caBlock, _ := pem.Decode(caPEM)
	certBlock, _ := pem.Decode(certificatePEM)
	keyBlock, _ := pem.Decode(privateKeyPEM)
	if caBlock == nil || certBlock == nil || keyBlock == nil {
		return errors.New("internal TLS Secret contains invalid PEM data")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !caCert.IsCA {
		return errors.New("internal TLS CA certificate is invalid")
	}
	leafCert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return errors.New("internal TLS server certificate is invalid")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return errors.New("internal TLS private key is invalid")
	}
	if err := leafCert.CheckSignatureFrom(caCert); err != nil {
		return fmt.Errorf("internal TLS server certificate is not signed by its CA: %w", err)
	}
	if !publicKeysEqual(leafCert.PublicKey, &key.PublicKey) {
		return errors.New("internal TLS certificate and private key do not match")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leafCert.Verify(x509.VerifyOptions{
		DNSName: options.ServiceName, Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("verify internal TLS certificate: %w", err)
	}
	return nil
}

func publicKeysEqual(left, right any) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && string(leftDER) == string(rightDER)
}

func serviceNames(service, namespace string) []string {
	return []string{
		service,
		service + "." + namespace + ".svc",
		service + "." + namespace + ".svc.cluster.local",
	}
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	const alphabet = "0123456789abcdef"
	encoded := make([]byte, size*2)
	for index, value := range data {
		encoded[index*2] = alphabet[value>>4]
		encoded[index*2+1] = alphabet[value&0x0f]
	}
	return string(encoded), nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	if serial.Sign() == 0 {
		return big.NewInt(1), nil
	}
	return serial, nil
}

func validateServiceDNS(service, namespace string) error {
	for _, name := range serviceNames(service, namespace) {
		if len(name) > 253 {
			return errors.New("internal Distribution service DNS name is invalid")
		}
		for _, label := range strings.Split(name, ".") {
			if len(label) > 63 || !dnsLabelPattern.MatchString(label) {
				return errors.New("internal Distribution service DNS name is invalid")
			}
		}
	}
	return nil
}
