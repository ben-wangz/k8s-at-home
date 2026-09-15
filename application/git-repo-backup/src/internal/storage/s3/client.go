// Package s3 implements the object-storage backend: conditional writes for
// ID claims and final objects, explicit multipart uploads, response-lost
// reconciliation, and paginated retention cleanup.
package s3

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"git-repo-backup/internal/config"
	"git-repo-backup/internal/observability"
)

// maxRetryAttempts bounds SDK retries for throttling and transient errors:
// the initial attempt plus three retries.
const maxRetryAttempts = 4

// Credentials files inside the read-only projected Secret.
const (
	accessKeyFile    = "access-key-id"
	secretKeyFile    = "secret-access-key"
	sessionTokenFile = "session-token"
	tokenFile        = "token"
)

// httpClient builds the shared HTTPS transport: the system CA pool plus the
// optional custom CA bundle appended, never replaced. TLS verification is
// never disabled.
func httpClient(caBundleFile string) (*awshttp.BuildableClient, error) {
	client := awshttp.NewBuildableClient()
	if caBundleFile == "" {
		return client, nil
	}
	pem, err := os.ReadFile(caBundleFile)
	if err != nil {
		return client, observability.WrapSafe(observability.CodeConfigInvalid,
			"read s3 ca bundle", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		return client, err
	}
	if !pool.AppendCertsFromPEM(pem) {
		return client, observability.WrapSafe(observability.CodeConfigInvalid,
			"s3 ca bundle contains no certificates", nil)
	}
	return client.WithTransportOptions(func(t *http.Transport) {
		t.TLSClientConfig = &tls.Config{RootCAs: pool}
	}), nil
}

func staticCredentials(dir string) (credentials.StaticCredentialsProvider, error) {
	id, err := os.ReadFile(filepath.Join(dir, accessKeyFile))
	if err != nil {
		return credentials.StaticCredentialsProvider{},
			observability.WrapSafe(observability.CodeConfigInvalid,
				"read s3 access key id", err)
	}
	secret, err := os.ReadFile(filepath.Join(dir, secretKeyFile))
	if err != nil {
		return credentials.StaticCredentialsProvider{},
			observability.WrapSafe(observability.CodeConfigInvalid,
				"read s3 secret access key", err)
	}
	token, _ := os.ReadFile(filepath.Join(dir, sessionTokenFile)) // optional
	return credentials.NewStaticCredentialsProvider(
		string(id), string(secret), string(token)), nil
}

// newClient assembles the S3 client. Credentials come exclusively from the
// configured mode; the SDK default chain (IMDS, shared files, env) is never
// consulted.
func newClient(ctx context.Context, cfg config.S3Storage) (*s3.Client, error) {
	hc, err := httpClient(cfg.CABundleFile)
	if err != nil {
		return nil, err
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithRetryMaxAttempts(maxRetryAttempts),
		awsconfig.WithHTTPClient(hc),
	}
	if cfg.Endpoint != "" {
		loadOpts = append(loadOpts, awsconfig.WithBaseEndpoint(cfg.Endpoint))
	}
	switch cfg.CredentialsMode {
	case "secret":
		prov, err := staticCredentials(cfg.CredentialsDir)
		if err != nil {
			return nil, err
		}
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(prov))
	case "workloadIdentity":
		// The STS call itself is authenticated by the projected web
		// identity token, not by SigV4 credentials.
		anonOpts := append(append([]func(*awsconfig.LoadOptions) error{}, loadOpts...),
			awsconfig.WithCredentialsProvider(aws.AnonymousCredentials{}))
		anonCfg, err := awsconfig.LoadDefaultConfig(ctx, anonOpts...)
		if err != nil {
			return nil, err
		}
		tokenPath := filepath.Join(cfg.WebIdentityTokenDir, tokenFile)
		if _, err := os.Stat(tokenPath); err != nil {
			return nil, observability.WrapSafe(observability.CodeConfigInvalid,
				"web identity token not projected", err)
		}
		prov := stscreds.NewWebIdentityRoleProvider(
			sts.NewFromConfig(anonCfg), cfg.RoleARN,
			stscreds.IdentityTokenFile(tokenPath))
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(prov))
	default:
		return nil, fmt.Errorf("unsupported credentials mode")
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, observability.WrapSafe(observability.CodeStorageFailed,
			"load s3 client config", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.UsePathStyle = cfg.ForcePathStyle
	}), nil
}
