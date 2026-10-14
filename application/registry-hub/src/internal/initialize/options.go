package initialize

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

const (
	defaultServiceAccount = "/var/run/secrets/kubernetes.io/serviceaccount"
	defaultRecordFile     = "/var/run/registry-hub-initialization/record.json"
	protocolVersion       = "v1"
	adminProtocolVersion  = "admin-v1"
)

var (
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)
	keyPattern      = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

type Options struct {
	Namespace       string
	Instance        string
	SecretName      string
	RecordName      string
	Scope           string
	ServiceName     string
	Auth            bool
	TLS             bool
	HTTPSecret      bool
	AuthKey         string
	HTPasswdKey     string
	CAKey           string
	CertificateKey  string
	PrivateKeyKey   string
	HTTPSecretKey   string
	AdminAuth       bool
	AdminSecretName string
	AdminRecordName string
	AdminScope      string
	AdminKey        string
}

func OptionsFromEnvironment() (Options, error) {
	options := Options{
		Namespace:       os.Getenv("POD_NAMESPACE"),
		Instance:        os.Getenv("REGISTRY_HUB_INIT_INSTANCE"),
		SecretName:      os.Getenv("REGISTRY_HUB_INIT_SECRET_NAME"),
		RecordName:      os.Getenv("REGISTRY_HUB_INIT_RECORD_NAME"),
		Scope:           os.Getenv("REGISTRY_HUB_INIT_SCOPE"),
		ServiceName:     os.Getenv("REGISTRY_HUB_INIT_SERVICE_NAME"),
		Auth:            strings.EqualFold(os.Getenv("REGISTRY_HUB_INIT_AUTH"), "true"),
		TLS:             strings.EqualFold(os.Getenv("REGISTRY_HUB_INIT_TLS"), "true"),
		HTTPSecret:      strings.EqualFold(os.Getenv("REGISTRY_HUB_INIT_HTTP_SECRET"), "true"),
		AuthKey:         os.Getenv("REGISTRY_HUB_INIT_AUTH_KEY"),
		HTPasswdKey:     os.Getenv("REGISTRY_HUB_INIT_HTPASSWD_KEY"),
		CAKey:           os.Getenv("REGISTRY_HUB_INIT_CA_KEY"),
		CertificateKey:  os.Getenv("REGISTRY_HUB_INIT_CERTIFICATE_KEY"),
		PrivateKeyKey:   os.Getenv("REGISTRY_HUB_INIT_PRIVATE_KEY_KEY"),
		HTTPSecretKey:   os.Getenv("REGISTRY_HUB_INIT_HTTP_SECRET_KEY"),
		AdminAuth:       strings.EqualFold(os.Getenv("REGISTRY_HUB_INIT_ADMIN_AUTH"), "true"),
		AdminSecretName: os.Getenv("REGISTRY_HUB_INIT_ADMIN_SECRET_NAME"),
		AdminRecordName: os.Getenv("REGISTRY_HUB_INIT_ADMIN_RECORD_NAME"),
		AdminScope:      os.Getenv("REGISTRY_HUB_INIT_ADMIN_SCOPE"),
		AdminKey:        os.Getenv("REGISTRY_HUB_INIT_ADMIN_KEY"),
	}
	if err := options.Validate(); err != nil {
		return Options{}, err
	}
	return options, nil
}

func (o Options) Validate() error {
	for name, value := range map[string]string{
		"namespace": o.Namespace, "instance": o.Instance,
	} {
		if value == "" {
			return fmt.Errorf("initialization %s is required", name)
		}
	}
	for name, value := range map[string]string{
		"namespace": o.Namespace, "instance": o.Instance,
	} {
		if len(value) > 63 || !dnsLabelPattern.MatchString(value) {
			return fmt.Errorf("initialization %s is not a valid Kubernetes DNS label", name)
		}
	}
	if !o.hasInternalConfiguration() && !o.AdminAuth {
		return errors.New("initialization has no generated configuration")
	}
	if o.hasInternalConfiguration() {
		if err := validateTarget("internal", o.SecretName, o.RecordName, o.Scope); err != nil {
			return err
		}
		if o.Auth != (o.AuthKey != "" && o.HTPasswdKey != "") {
			return errors.New("internal initialization authentication keys are incomplete")
		}
		if o.TLS != (o.CAKey != "" && o.CertificateKey != "" && o.PrivateKeyKey != "") {
			return errors.New("internal initialization TLS keys are incomplete")
		}
		if o.HTTPSecret != (o.HTTPSecretKey != "") {
			return errors.New("internal initialization HTTP secret key is incomplete")
		}
		if o.TLS && o.ServiceName == "" {
			return errors.New("internal initialization service name is required for TLS")
		}
		keys := make(map[string]struct{})
		for _, key := range o.dataKeys() {
			if len(key) > 253 || !keyPattern.MatchString(key) {
				return fmt.Errorf("internal initialization key %q is invalid", key)
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("internal initialization keys must be unique: %q", key)
			}
			keys[key] = struct{}{}
		}
	}
	if o.AdminAuth {
		if err := validateTarget("admin", o.AdminSecretName, o.AdminRecordName, o.AdminScope); err != nil {
			return err
		}
		if len(o.AdminKey) > 253 || !keyPattern.MatchString(o.AdminKey) {
			return fmt.Errorf("admin initialization key %q is invalid", o.AdminKey)
		}
		if o.hasInternalConfiguration() && o.SecretName == o.AdminSecretName {
			return errors.New("internal and admin initialization Secret names must be different")
		}
		if o.hasInternalConfiguration() && o.RecordName == o.AdminRecordName {
			return errors.New("internal and admin initialization record names must be different")
		}
	}
	return nil
}

func (o Options) hasInternalConfiguration() bool {
	return o.Auth || o.TLS || o.HTTPSecret
}

func validateTarget(kind, secretName, recordName, scope string) error {
	for name, value := range map[string]string{"secret name": secretName, "record name": recordName, "scope": scope} {
		if value == "" {
			return fmt.Errorf("%s initialization %s is required", kind, name)
		}
	}
	for name, value := range map[string]string{"secret name": secretName, "record name": recordName} {
		if len(value) > 63 || !dnsLabelPattern.MatchString(value) {
			return fmt.Errorf("%s initialization %s is not a valid Kubernetes DNS label", kind, name)
		}
	}
	if len(scope) != 64 {
		return fmt.Errorf("%s initialization scope must be a SHA-256 hex digest", kind)
	}
	if _, err := hex.DecodeString(scope); err != nil {
		return fmt.Errorf("%s initialization scope must be a SHA-256 hex digest", kind)
	}
	return nil
}

func (o Options) dataKeys() []string {
	keys := make([]string, 0, 6)
	if o.Auth {
		keys = append(keys, o.AuthKey, o.HTPasswdKey)
	}
	if o.TLS {
		keys = append(keys, o.CAKey, o.CertificateKey, o.PrivateKeyKey)
	}
	if o.HTTPSecret {
		keys = append(keys, o.HTTPSecretKey)
	}
	return keys
}
