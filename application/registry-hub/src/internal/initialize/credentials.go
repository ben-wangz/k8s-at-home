package initialize

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"golang.org/x/crypto/bcrypt"
	"registry-hub/internal/auth"
	"strings"
	"time"
)

type distributionCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token,omitempty"`
}

type adminAuthConfig struct {
	Basic  []adminCredential `json:"basic"`
	Bearer []adminCredential `json:"bearer,omitempty"`
}

type adminCredential struct {
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Token       string   `json:"token,omitempty"`
	Permissions []string `json:"permissions"`
}

func generateAdminAuthData(key string) (map[string][]byte, error) {
	password, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("generate admin password: %w", err)
	}
	configuration := adminAuthConfig{Basic: []adminCredential{{
		Username: "admin", Password: password,
		Permissions: []string{auth.TaskAdmin, auth.ReferrerPrune, auth.GCAdmin},
	}}}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, fmt.Errorf("encode admin credentials: %w", err)
	}
	return map[string][]byte{key: encoded}, nil
}

func validateAdminAuthData(data map[string][]byte, key string) error {
	if len(data) != 1 || len(data[key]) == 0 {
		return errors.New("admin auth Secret must contain only its configured key")
	}
	var configuration adminAuthConfig
	if err := json.Unmarshal(data[key], &configuration); err != nil {
		return errors.New("admin auth configuration is invalid")
	}
	if len(configuration.Basic) != 1 || len(configuration.Bearer) != 0 {
		return errors.New("admin auth configuration must contain exactly one Basic credential")
	}
	credential := configuration.Basic[0]
	if credential.Username != "admin" || len(credential.Password) != 64 || credential.Token != "" {
		return errors.New("admin auth credential is invalid")
	}
	if _, err := hex.DecodeString(credential.Password); err != nil {
		return errors.New("admin auth password is not hexadecimal")
	}
	expected := map[string]struct{}{
		auth.TaskAdmin: {}, auth.ReferrerPrune: {}, auth.GCAdmin: {},
	}
	if len(credential.Permissions) != len(expected) {
		return errors.New("admin auth permissions are invalid")
	}
	for _, permission := range credential.Permissions {
		if _, ok := expected[permission]; !ok {
			return errors.New("admin auth permissions are invalid")
		}
		delete(expected, permission)
	}
	if len(expected) != 0 {
		return errors.New("admin auth permissions are incomplete")
	}
	return nil
}

func generateSecretData(options Options) (map[string][]byte, error) {
	data := make(map[string][]byte)
	if options.Auth {
		if err := generateAuthentication(data, options); err != nil {
			return nil, err
		}
	}
	if options.TLS {
		if err := generateTLS(data, options); err != nil {
			return nil, err
		}
	}
	if options.HTTPSecret {
		value, err := randomHex(32)
		if err != nil {
			return nil, fmt.Errorf("generate Distribution HTTP secret: %w", err)
		}
		data[options.HTTPSecretKey] = []byte(value)
	}
	return data, nil
}

func generateAuthentication(data map[string][]byte, options Options) error {
	password, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("generate Distribution password: %w", err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash Distribution password: %w", err)
	}
	credentials := distributionCredentials{Username: "registry-hub-backend", Password: password}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("encode Distribution credentials: %w", err)
	}
	data[options.AuthKey] = encoded
	data[options.HTPasswdKey] = []byte(credentials.Username + ":" + string(hash) + "\n")
	return nil
}

func generateTLS(data map[string][]byte, options Options) error {
	now := time.Now().UTC()
	caKey, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return fmt.Errorf("generate internal CA key: %w", err)
	}
	caSerial, err := randomSerial()
	if err != nil {
		return fmt.Errorf("generate internal CA serial: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "registry-hub internal CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create internal CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return fmt.Errorf("parse internal CA certificate: %w", err)
	}
	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("generate internal server key: %w", err)
	}
	serverSerial, err := randomSerial()
	if err != nil {
		return fmt.Errorf("generate internal server certificate serial: %w", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: options.ServiceName},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     serviceNames(options.ServiceName, options.Namespace),
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create internal server certificate: %w", err)
	}
	serverKeyDER := x509.MarshalPKCS1PrivateKey(serverKey)
	data[options.CAKey] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	data[options.CertificateKey] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	data[options.PrivateKeyKey] = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: serverKeyDER})
	return nil
}

func validateSecretData(data map[string][]byte, options Options) error {
	for _, key := range options.dataKeys() {
		if len(data[key]) == 0 {
			return fmt.Errorf("internal Secret is missing required key %q", key)
		}
	}
	if options.Auth {
		if err := validateAuthentication(data[options.AuthKey], data[options.HTPasswdKey]); err != nil {
			return err
		}
	}
	if options.TLS {
		if err := validateTLS(data[options.CAKey], data[options.CertificateKey], data[options.PrivateKeyKey], options); err != nil {
			return err
		}
	}
	if options.HTTPSecret {
		value := data[options.HTTPSecretKey]
		if len(value) != 64 {
			return errors.New("internal Distribution HTTP secret has an invalid length")
		}
		if _, err := hex.DecodeString(string(value)); err != nil {
			return errors.New("internal Distribution HTTP secret is not hexadecimal")
		}
	}
	return nil
}

func validateAuthentication(authJSON, htpasswd []byte) error {
	var credentials distributionCredentials
	if err := json.Unmarshal(authJSON, &credentials); err != nil || credentials.Username == "" || credentials.Password == "" || credentials.Token != "" {
		return errors.New("internal Distribution authentication JSON is invalid")
	}
	matched := false
	for _, line := range strings.Split(string(htpasswd), "\n") {
		username, hash, found := strings.Cut(line, ":")
		if found && username == credentials.Username && bcrypt.CompareHashAndPassword([]byte(hash), []byte(credentials.Password)) == nil {
			matched = true
		}
	}
	if !matched {
		return errors.New("internal Distribution credentials do not match the htpasswd entry")
	}
	return nil
}
