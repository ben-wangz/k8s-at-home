package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

func (c *Config) validateS3() error {
	s3 := c.Storage.S3
	if s3.Bucket == "" {
		return fmt.Errorf("storage.s3.bucket is required")
	}
	if strings.Contains(s3.Bucket, "/") || strings.HasPrefix(s3.Bucket, "s3://") {
		return fmt.Errorf("storage.s3.bucket must be a bare bucket name")
	}
	if s3.Region == "" {
		return fmt.Errorf("storage.s3.region is required")
	}
	if err := validatePrefix(s3.Prefix); err != nil {
		return err
	}
	if err := validateEndpoint(s3.Endpoint, s3.AllowInsecureHTTP); err != nil {
		return err
	}
	switch s3.CredentialsMode {
	case "secret":
	case "workloadIdentity":
		if s3.RoleARN == "" {
			return fmt.Errorf("storage.s3.roleArn is required for workloadIdentity mode")
		}
	default:
		return fmt.Errorf("storage.s3.credentialsMode must be secret or workloadIdentity")
	}
	switch s3.ServerSideEncrypt {
	case "":
		if s3.KMSKeyID != "" {
			return fmt.Errorf("storage.s3.kmsKeyId must be empty unless serverSideEncryption is aws:kms")
		}
	case "AES256":
		if s3.KMSKeyID != "" {
			return fmt.Errorf("storage.s3.kmsKeyId must be empty unless serverSideEncryption is aws:kms")
		}
	case "aws:kms":
		if s3.KMSKeyID == "" {
			return fmt.Errorf("storage.s3.kmsKeyId is required when serverSideEncryption is aws:kms")
		}
	default:
		return fmt.Errorf("storage.s3.serverSideEncryption must be empty, AES256, or aws:kms")
	}
	return nil
}

func validatePrefix(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("storage.s3.prefix is required")
	}
	if prefix != strings.TrimSpace(prefix) || strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		return fmt.Errorf("storage.s3.prefix must be relative without leading or trailing slashes")
	}
	if strings.Contains(prefix, "\\") || strings.Contains(prefix, "//") {
		return fmt.Errorf("storage.s3.prefix must not contain backslashes or empty segments")
	}
	for _, r := range prefix {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("storage.s3.prefix must not contain control characters")
		}
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("storage.s3.prefix must not contain '.' or '..' segments")
		}
	}
	return nil
}

// validateEndpoint accepts the empty string (regional AWS endpoints) or a
// bare HTTPS URL without userinfo, path, query, or fragment. HTTP is accepted
// only when explicitly enabled for an isolated S3-compatible endpoint.
func validateEndpoint(endpoint string, allowInsecureHTTP bool) error {
	if endpoint == "" {
		return nil
	}
	u, err := parseURLStrict(endpoint)
	if err != nil {
		return err
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !allowInsecureHTTP {
			return fmt.Errorf("storage.s3.endpoint must use https unless storage.s3.allowInsecureHttp is true")
		}
	default:
		return fmt.Errorf("storage.s3.endpoint must use https or explicitly allow http")
	}
	if u.User != nil {
		return fmt.Errorf("storage.s3.endpoint must not contain userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return fmt.Errorf("storage.s3.endpoint must not contain a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("storage.s3.endpoint must not contain a query or fragment")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("storage.s3.endpoint must contain a host")
	}
	return nil
}

func (c *Config) validateDurations() error {
	var err error
	if c.MaxRunDuration, err = time.ParseDuration(c.Backup.MaxRunDuration); err != nil {
		return fmt.Errorf("backup.maxRunDuration: %w", errDuration())
	}
	if c.GitTimeout, err = time.ParseDuration(c.Backup.GitTimeout); err != nil {
		return fmt.Errorf("backup.gitTimeout: %w", errDuration())
	}
	if c.MaxRunDuration <= 0 || c.GitTimeout <= 0 {
		return fmt.Errorf("backup.maxRunDuration and backup.gitTimeout must be positive")
	}
	if c.Retention.MaxAge != "" {
		if c.MaxAge, err = time.ParseDuration(c.Retention.MaxAge); err != nil {
			return fmt.Errorf("retention.maxAge: %w", errDuration())
		}
		if c.MaxAge <= 0 {
			return fmt.Errorf("retention.maxAge must be positive when set")
		}
	}
	if c.IncompleteMaxAge, err = time.ParseDuration(c.Retention.IncompleteMaxAge); err != nil {
		return fmt.Errorf("retention.incompleteMaxAge: %w", errDuration())
	}
	if c.IncompleteMaxAge <= 0 {
		return fmt.Errorf("retention.incompleteMaxAge must be positive")
	}
	if c.Retention.Enabled && c.Retention.MaxBackups == 0 && c.Retention.MaxAge == "" {
		return fmt.Errorf("retention requires maxBackups or maxAge when enabled")
	}
	if c.Retention.MaxBackups < 0 {
		return fmt.Errorf("retention.maxBackups must not be negative")
	}
	// Keep incomplete cleanup from deleting runs that are still executing.
	if c.IncompleteMaxAge <= c.MaxRunDuration+10*time.Minute {
		return fmt.Errorf("retention.incompleteMaxAge must exceed backup.maxRunDuration by at least 10m")
	}
	return nil
}

// errDuration hides the raw ParseDuration message, which echoes the input.
func errDuration() error {
	return fmt.Errorf("invalid duration (use Go duration syntax such as 24h, 30m, 10s)")
}

func parseURLStrict(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("storage.s3.endpoint is not a valid URL")
	}
	return u, nil
}
