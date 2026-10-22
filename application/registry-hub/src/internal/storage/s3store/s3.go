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
	partSize           int64
	multipartThreshold int64
	httpClient         *http.Client
	replayMu           sync.Mutex
	replay             *replayManager
	replayClosed       bool
	multipartMu        sync.Mutex
	multipartActive    map[string]int
	multipartCleaning  map[string]bool
	multipartChanged   chan struct{}
}

func New(ctx context.Context, cfg config.S3) (*Store, error) {
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
	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(cfg.Region),
		awsconfig.WithHTTPClient(httpClient),
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
	replay, err := newReplayManager(replayConfig{
		perSourceMemory: cfg.ReplayMemoryBytes,
		maxMemory:       cfg.ReplayMaxMemoryBytes,
		maxDisk:         cfg.ReplayMaxDiskBytes,
		directory:       cfg.ReplayDirectory,
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &Store{
		client:             client,
		bucket:             cfg.Bucket,
		prefix:             strings.Trim(cfg.Prefix, "/"),
		partSize:           cfg.PartSize,
		multipartThreshold: cfg.MultipartThreshold,
		httpClient:         httpClient,
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

func (s *Store) Get(ctx context.Context, key string) (storage.Object, error) {
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Object{}, err
	}
	result, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)})
	if err != nil {
		return storage.Object{}, mapError(err)
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		_ = result.Body.Close()
		return storage.Object{}, errors.New("S3 object did not provide an ETag for conditional operations")
	}
	return storage.Object{Body: result.Body, Size: aws.ToInt64(result.ContentLength), Version: version, ModTime: aws.ToTime(result.LastModified).UnixNano()}, nil
}

func (s *Store) Head(ctx context.Context, key string) (storage.Entry, error) {
	name, err := s.objectKey(key)
	if err != nil {
		return storage.Entry{}, err
	}
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(name)})
	if err != nil {
		return storage.Entry{}, mapError(err)
	}
	version := aws.ToString(result.ETag)
	if version == "" {
		return storage.Entry{}, errors.New("S3 object did not provide an ETag for conditional operations")
	}
	return storage.Entry{Key: key, Version: version, Size: aws.ToInt64(result.ContentLength), ModTime: aws.ToTime(result.LastModified).UnixNano()}, nil
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
	source, err := replayManager.prepare(ctx, body, size)
	if err != nil {
		return storage.Entry{}, err
	}
	defer source.Close()
	if size >= s.multipartThreshold && size > 0 {
		return s.putMultipart(ctx, key, name, source, options)
	}
	return s.putSingle(ctx, key, name, source, options)
}
