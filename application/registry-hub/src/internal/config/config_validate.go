package config

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"registry-hub/internal/route"
	"strings"
)

func (c Config) Validate() error {
	if c.GCMode != "disabled" && c.GCMode != "local" && c.GCMode != "kubernetes" {
		return errors.New("REGISTRY_HUB_GC_MODE must be disabled, local, or kubernetes")
	}
	if c.GCMode == "local" && (!filepath.IsAbs(c.GCExecutable) || !filepath.IsAbs(c.GCConfig)) {
		return errors.New("local GC requires absolute REGISTRY_HUB_GC_EXECUTABLE and REGISTRY_HUB_GC_CONFIG paths")
	}
	if c.GCMode == "disabled" && (c.GCLifecycleExecutable != "" || c.GCLifecycleConfig != "") {
		return errors.New("Distribution lifecycle adapter requires local or Kubernetes GC")
	}
	if c.GCLifecycleExecutable != "" && !filepath.IsAbs(c.GCLifecycleExecutable) {
		return errors.New("REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_EXECUTABLE must be an absolute path")
	}
	if c.GCLifecycleConfig != "" && !filepath.IsAbs(c.GCLifecycleConfig) {
		return errors.New("REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_CONFIG must be an absolute path")
	}
	if c.GCLifecycleConfig != "" && c.GCLifecycleExecutable == "" {
		return errors.New("REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_CONFIG requires a lifecycle executable")
	}
	if c.GCMode == "kubernetes" && (c.GCImage == "" || c.GCConfigMap == "" || c.GCConfigDigest == "" || c.GCNamespace == "") {
		return errors.New("Kubernetes GC requires a Job image, config ConfigMap digest, and POD_NAMESPACE")
	}
	seenImagePullSecrets := make(map[string]struct{}, len(c.GCImagePullSecrets))
	for _, secret := range c.GCImagePullSecrets {
		if !validKubernetesObjectName(secret.Name) {
			return errors.New("REGISTRY_HUB_GC_JOB_IMAGE_PULL_SECRETS contains an invalid Secret name")
		}
		if _, found := seenImagePullSecrets[secret.Name]; found {
			return errors.New("REGISTRY_HUB_GC_JOB_IMAGE_PULL_SECRETS contains duplicate Secret names")
		}
		seenImagePullSecrets[secret.Name] = struct{}{}
	}
	if c.StorageBackend != "filesystem" && c.StorageBackend != "s3" {
		return fmt.Errorf("unsupported storage backend %q", c.StorageBackend)
	}
	if c.StorageBackend == "filesystem" && !filepath.IsAbs(c.FilesystemRoot) {
		return errors.New("REGISTRY_HUB_FILESYSTEM_ROOT must be absolute")
	}
	if c.StorageBackend == "s3" && c.S3.Bucket == "" {
		return errors.New("REGISTRY_HUB_S3_BUCKET is required for S3 storage")
	}
	if c.AdminAuth.Empty() {
		return errors.New("REGISTRY_HUB_ADMIN_AUTH_CONFIG is required")
	}
	if (c.DistributionAuth.Username == "") != (c.DistributionAuth.Password == "") {
		return errors.New("Distribution username and password must be configured together")
	}
	if c.DistributionAuth.Token != "" && c.DistributionAuth.Username != "" {
		return errors.New("configure either Distribution Basic credentials or a Bearer token, not both")
	}
	for _, endpoint := range c.DistributionRedirectEndpoints {
		parsed, err := url.Parse(endpoint)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || parsed.Scheme != "https" && parsed.Scheme != "http" {
			return fmt.Errorf("invalid Distribution redirect endpoint %q", endpoint)
		}
	}
	if c.DistributionAuth.Username == "" && c.DistributionAuth.Token == "" {
		return errors.New("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG must configure Basic credentials or a Bearer token")
	}
	if c.GCMode != "disabled" && (!c.GCUniqueWriterConfirmed || !c.GCUploadPurgingDisabled) {
		return errors.New("GC requires confirmed unique Distribution writer and disabled upload purging")
	}
	if c.S3.MultipartThreshold < 5<<20 || c.S3.PartSize < 5<<20 || c.S3.PartSize > 5<<30 {
		return errors.New("S3 multipart threshold and part size must be at least 5 MiB, and part size cannot exceed 5 GiB")
	}
	if c.S3.Timeout <= 0 || c.S3.MaxConnections < 1 {
		return errors.New("S3 timeout and maximum connections must be positive")
	}
	if c.S3.Endpoint != "" {
		endpoint, err := url.Parse(c.S3.Endpoint)
		if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") {
			return errors.New("REGISTRY_HUB_S3_ENDPOINT must be an absolute HTTP(S) URL")
		}
		if endpoint.Scheme == "http" && !c.S3.AllowHTTP {
			return errors.New("S3 HTTP requires REGISTRY_HUB_S3_ALLOW_HTTP=true")
		}
	}
	if c.Limits.ManifestBytes < 1 || c.Limits.UploadBytes < 1 || c.Limits.ObjectBytes < 1 || c.Limits.ClosureObjects < 1 || c.Limits.ClosureBytes < 1 || c.Limits.ClosureDepth < 1 || c.Limits.MaxDescriptors < 1 || c.Limits.DescriptorDataBytes < 1 || c.Limits.Workers < 1 || c.Limits.Workers > 4 || c.Limits.PendingTasks < 1 || c.Limits.MaxPageSize < 1 || c.Limits.MaxReferrerPages < 1 || c.Limits.UploadIdleTimeout <= 0 || c.Limits.UploadMaxLifetime <= 0 || c.Limits.GlobalRate <= 0 || c.Limits.GlobalBurst < 1 || c.Limits.NoAuthRate <= 0 || c.Limits.NoAuthBurst < 1 || c.Limits.RequestTimeout <= 0 {
		return errors.New("resource limits must be positive")
	}
	for authority, auth := range c.UpstreamAuth {
		if routeAuthorityError := validateAuthority(authority); routeAuthorityError != nil {
			return fmt.Errorf("invalid upstream auth authority %q", authority)
		}
		if auth.Type != "basic" && auth.Type != "bearer" {
			return fmt.Errorf("upstream auth type for %q must be basic or bearer", authority)
		}
		if auth.Type == "basic" && auth.Username == "" {
			return fmt.Errorf("upstream basic auth for %q requires username", authority)
		}
		if auth.Type == "bearer" && auth.Token == "" {
			return fmt.Errorf("upstream bearer auth for %q requires token", authority)
		}
	}
	for authority := range c.TransportRules {
		if err := validateAuthority(authority); err != nil {
			return fmt.Errorf("invalid upstream transport authority %q", authority)
		}
	}
	for authority, proxy := range c.Proxy.Authorities {
		if err := validateAuthority(authority); err != nil {
			return fmt.Errorf("invalid proxy authority %q", authority)
		}
		if err := validateProxyURL(proxy); err != nil {
			return fmt.Errorf("invalid proxy URL for %q", authority)
		}
	}
	if c.Proxy.Default != "" {
		if err := validateProxyURL(c.Proxy.Default); err != nil {
			return fmt.Errorf("invalid default proxy URL: %w", err)
		}
	}
	if err := validateAuth(c.ClientAuth); err != nil {
		return fmt.Errorf("client auth config: %w", err)
	}
	if err := validateAuth(c.AdminAuth); err != nil {
		return fmt.Errorf("admin auth config: %w", err)
	}
	return nil
}

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

func validateAuth(auth Auth) error {
	const allowed = "pull catalog push delete task-admin referrer-prune gc-admin"
	for _, credential := range append(append([]Credential{}, auth.Basic...), auth.Bearer...) {
		if len(credential.Permissions) == 0 {
			return errors.New("each credential must declare at least one permission")
		}
		for _, permission := range credential.Permissions {
			if !strings.Contains(" "+allowed+" ", " "+permission+" ") {
				return fmt.Errorf("unknown permission %q", permission)
			}
		}
	}
	for _, credential := range auth.Basic {
		if credential.Username == "" || credential.Password == "" {
			return errors.New("Basic credentials require username and password")
		}
	}
	for _, credential := range auth.Bearer {
		if credential.Token == "" {
			return errors.New("Bearer credentials require token")
		}
	}
	return nil
}
