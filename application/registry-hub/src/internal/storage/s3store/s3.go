package s3store

import (
	"context"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"net/url"
	"registry-hub/internal/config"
	"registry-hub/internal/storage"
	"strings"
	"sync"
	"time"
)

type Store struct {
	client             *s3.Client
	bucket             string
	prefix             string
	readOnly           bool
	partSize           int64
	multipartThreshold int64
	httpClient         *http.Client
	attempts           *attemptTransport
	replayMu           sync.Mutex
	replay             *replayManager
	replayClosed       bool
	checksumSHA256     bool
	multipartMu        sync.Mutex
	multipartActive    map[string]int
	multipartCleaning  map[string]bool
	multipartChanged   chan struct{}
}

func New(ctx context.Context, cfg config.S3) (*Store, error) {
	return newStore(ctx, cfg, false)
}

func NewReadOnly(ctx context.Context, cfg config.S3) (*Store, error) {
	return newStore(ctx, cfg, true)
}

func newStore(ctx context.Context, cfg config.S3, readOnly bool) (*Store, error) {
	if cfg.Prefix != "" && !validKey(cfg.Prefix) {
		return nil, errors.New("S3 prefix must be a relative object-key prefix")
	}
	if err := validateEndpoint(cfg.Endpoint, cfg.AllowHTTP); err != nil {
		return nil, err
	}
	tlsConfig, err := config.LoadTLS(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load S3 CA: %w", err)
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxConnsPerHost:       cfg.MaxConnections,
		MaxIdleConns:          cfg.MaxConnections * 2,
		MaxIdleConnsPerHost:   cfg.MaxConnections,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	httpClient := &http.Client{Transport: transport, Timeout: cfg.Timeout}
	domain := "state"
	if readOnly {
		domain = "registry"
	}
	attempts := &attemptTransport{base: transport, domain: domain}
	sdkHTTPClient := &http.Client{Transport: attempts, Timeout: cfg.Timeout}
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithHTTPClient(sdkHTTPClient),
	}
	if cfg.AccessKeyID != "" || cfg.SecretAccessKey != "" {
		if cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
			return nil, errors.New("both S3 inspection credential fields are required")
		}
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey, Source: "registry-inspection"}, nil
		})))
	}
	if strings.HasPrefix(cfg.Endpoint, "http://") {
		loadOptions = append(loadOptions, awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("load S3 credentials and configuration: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		if options.Retryer != nil {
			options.Retryer = trackingRetryer{delegate: options.Retryer}
		}
		options.UsePathStyle = cfg.UsePathStyle
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(strings.TrimSuffix(cfg.Endpoint, "/"))
		}
	})
	checksumSHA256 := supportsAWSChecksumSHA256(cfg.Endpoint)
	var replay *replayManager
	if !readOnly {
		replay, err = newReplayManager(replayConfig{
			perSourceMemory: cfg.ReplayMemoryBytes,
			maxMemory:       cfg.ReplayMaxMemoryBytes,
			maxDisk:         cfg.ReplayMaxDiskBytes,
			directory:       cfg.ReplayDirectory,
		})
		if err != nil {
			transport.CloseIdleConnections()
			return nil, err
		}
	}
	return &Store{
		client:             client,
		bucket:             cfg.Bucket,
		prefix:             strings.Trim(cfg.Prefix, "/"),
		readOnly:           readOnly,
		partSize:           cfg.PartSize,
		multipartThreshold: cfg.MultipartThreshold,
		httpClient:         httpClient,
		attempts:           attempts,
		checksumSHA256:     checksumSHA256,
		replay:             replay,
	}, nil
}

func validateEndpoint(raw string, allowHTTP bool) error {
	if raw == "" {
		return nil
	}
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Scheme != "https" && endpoint.Scheme != "http" {
		return errors.New("S3 endpoint must be an absolute HTTP(S) URL without userinfo, query, or fragment")
	}
	if endpoint.Scheme == "http" && !allowHTTP {
		return errors.New("S3 HTTP requires explicit AllowHTTP")
	}
	return nil
}

func supportsAWSChecksumSHA256(endpoint string) bool {
	if endpoint == "" {
		return true
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return strings.HasSuffix(host, ".amazonaws.com") || strings.HasSuffix(host, ".amazonaws.com.cn") || host == "amazonaws.com" || host == "amazonaws.com.cn"
}

func (s *Store) ReplayUsage() (int64, int64, int) {
	s.replayMu.Lock()
	replay := s.replay
	s.replayMu.Unlock()
	if replay == nil {
		return 0, 0, 0
	}
	return replay.usage()
}

func (s *Store) Put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options, "")
}

func (s *Store) PutVerified(ctx context.Context, key string, body io.Reader, size int64, options storage.VerifiedPutOptions) (storage.Entry, error) {
	return s.put(ctx, key, body, size, options.PutOptions, options.Digest)
}

func (s *Store) put(ctx context.Context, key string, body io.Reader, size int64, options storage.PutOptions, expectedDigest string) (storage.Entry, error) {
	if s.readOnly {
		return storage.Entry{}, storage.ErrPermissionDenied
	}
	if options.IfAbsent && options.IfMatch != "" {
		return storage.Entry{}, errors.New("conditional create and conditional update are mutually exclusive")
	}
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Entry{}, err
	}
	if size < 0 {
		return storage.Entry{}, errors.New("S3 object size cannot be negative")
	}
	replayManager, err := s.ensureReplayManager()
	if err != nil {
		return storage.Entry{}, err
	}
	source, err := replayManager.prepareVerified(ctx, body, size, expectedDigest)
	if err != nil {
		return storage.Entry{}, err
	}
	defer source.Close()
	if size >= s.multipartThreshold && size > 0 {
		return s.putMultipart(ctx, key, name, source, options)
	}
	return s.putSingle(ctx, key, name, source, options)
}
