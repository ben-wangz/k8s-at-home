package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"registry-hub/internal/route"
	"strings"
)

func validKubernetesObjectName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) == 0 || len(label) > 63 || !isLowerAlphaNumeric(label[0]) || !isLowerAlphaNumeric(label[len(label)-1]) {
			return false
		}
		for _, char := range label {
			if char != '-' && !isLowerAlphaNumeric(byte(char)) {
				return false
			}
		}
	}
	return true
}

func isLowerAlphaNumeric(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9'
}

func LoadTLS(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(data) {
		return nil, errors.New("CA file contains no usable certificates")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

func loadJSON[T any](name string) (T, error) {
	var zero T
	value := os.Getenv(name)
	if value == "" {
		return zero, nil
	}
	if strings.HasPrefix(value, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(value, "@"))
		if err != nil {
			return zero, fmt.Errorf("read %s file: %w", name, err)
		}
		value = string(data)
	}
	var parsed T
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return zero, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

func value(name, fallback string) string {
	if current := os.Getenv(name); current != "" {
		return current
	}
	return fallback
}

func validateAuthority(authority string) error {
	return route.ValidateAuthority(authority)
}

func validateProxyURL(value string) error {
	proxy, err := url.Parse(value)
	if err != nil || proxy.Host == "" || (proxy.Scheme != "http" && proxy.Scheme != "https") || proxy.Path != "" && proxy.Path != "/" || proxy.RawQuery != "" || proxy.Fragment != "" {
		return errors.New("proxy must be an absolute HTTP(S) URL")
	}
	return nil
}
