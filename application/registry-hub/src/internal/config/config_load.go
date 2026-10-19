package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func Load() (Config, error) {
	var cfg Config
	var parseErrors error
	intValue := func(name string, fallback int64) int64 {
		current := os.Getenv(name)
		if current == "" {
			return fallback
		}
		parsed, err := strconv.ParseInt(current, 10, 64)
		if err != nil {
			parseErrors = errors.Join(parseErrors, fmt.Errorf("parse %s as integer: %w", name, err))
			return fallback
		}
		return parsed
	}
	secondsValue := func(name string, fallback int64) time.Duration {
		seconds := intValue(name, fallback)
		if seconds > math.MaxInt64/int64(time.Second) || seconds < math.MinInt64/int64(time.Second) {
			parseErrors = errors.Join(parseErrors, fmt.Errorf("%s duration overflows time.Duration", name))
			seconds = fallback
		}
		return time.Duration(seconds) * time.Second
	}
	cfg.ClientAddr = value("REGISTRY_HUB_CLIENT_ADDR", ":5000")
	cfg.AdminAddr = value("REGISTRY_HUB_ADMIN_ADDR", ":8081")
	cfg.StorageBackend = strings.ToLower(value("REGISTRY_HUB_STORAGE_BACKEND", "filesystem"))
	cfg.FilesystemRoot = value("REGISTRY_HUB_FILESYSTEM_ROOT", "/var/lib/registry-hub")
	cfg.InternalToken = os.Getenv("REGISTRY_HUB_INTERNAL_TOKEN")
	cfg.GCMode = strings.ToLower(value("REGISTRY_HUB_GC_MODE", "disabled"))
	cfg.GCExecutable = os.Getenv("REGISTRY_HUB_GC_EXECUTABLE")
	cfg.GCConfig = os.Getenv("REGISTRY_HUB_GC_CONFIG")
	cfg.GCLifecycleExecutable = os.Getenv("REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_EXECUTABLE")
	cfg.GCLifecycleConfig = os.Getenv("REGISTRY_HUB_DISTRIBUTION_LIFECYCLE_CONFIG")
	cfg.GCImage = os.Getenv("REGISTRY_HUB_GC_JOB_IMAGE")
	imagePullSecrets, imagePullSecretsErr := loadJSON[[]ImagePullSecretReference]("REGISTRY_HUB_GC_JOB_IMAGE_PULL_SECRETS")
	cfg.GCImagePullSecrets = imagePullSecrets
	if imagePullSecretsErr != nil {
		parseErrors = errors.Join(parseErrors, imagePullSecretsErr)
	}
	cfg.GCConfigMap = os.Getenv("REGISTRY_HUB_GC_JOB_CONFIGMAP")
	cfg.GCConfigDigest = os.Getenv("REGISTRY_HUB_GC_JOB_CONFIG_DIGEST")
	cfg.GCEnvSecret = os.Getenv("REGISTRY_HUB_GC_JOB_ENV_SECRET")
	cfg.GCConfigKey = value("REGISTRY_HUB_GC_JOB_CONFIG_KEY", "config.yml")
	cfg.GCStoragePVC = os.Getenv("REGISTRY_HUB_GC_JOB_STORAGE_PVC")
	cfg.GCServiceAccount = os.Getenv("REGISTRY_HUB_GC_JOB_SERVICE_ACCOUNT")
	cfg.GCNamespace = os.Getenv("POD_NAMESPACE")
	cfg.GCUniqueWriterConfirmed = strings.EqualFold(os.Getenv("REGISTRY_HUB_GC_UNIQUE_WRITER_CONFIRMED"), "true")
	cfg.GCUploadPurgingDisabled = strings.EqualFold(os.Getenv("REGISTRY_HUB_GC_UPLOAD_PURGING_DISABLED"), "true")
	cfg.DistributionCA = value("REGISTRY_HUB_DISTRIBUTION_CA", os.Getenv("REGISTRY_HUB_DISTRIBUTION_CA_FILE"))
	cfg.CatalogEnabled = strings.EqualFold(os.Getenv("REGISTRY_HUB_CATALOG_ENABLED"), "true")
	objectBytes := intValue("REGISTRY_HUB_LIMIT_OBJECT_BYTES", 100<<30)
	defaultDownloadMaxBytes := int64(0)
	if objectBytes > math.MaxInt64/2 || objectBytes < 0 {
		parseErrors = errors.Join(parseErrors, errors.New("REGISTRY_HUB_LIMIT_OBJECT_BYTES cannot be doubled safely for the default download quota"))
	} else {
		defaultDownloadMaxBytes = 2 * objectBytes
	}
	downloadMaxBytes := intValue("REGISTRY_HUB_DOWNLOAD_MAX_BYTES", defaultDownloadMaxBytes)
	cfg.Limits = Limits{
		ManifestBytes:           intValue("REGISTRY_HUB_LIMIT_MANIFEST_BYTES", 16<<20),
		UploadBytes:             intValue("REGISTRY_HUB_LIMIT_UPLOAD_BYTES", 256<<20),
		ObjectBytes:             objectBytes,
		ClosureObjects:          int(intValue("REGISTRY_HUB_LIMIT_CLOSURE_OBJECTS", 10000)),
		ClosureBytes:            intValue("REGISTRY_HUB_LIMIT_CLOSURE_BYTES", 1<<40),
		ClosureDepth:            int(intValue("REGISTRY_HUB_LIMIT_CLOSURE_DEPTH", 64)),
		MaxDescriptors:          int(intValue("REGISTRY_HUB_LIMIT_DESCRIPTORS", 10000)),
		DescriptorDataBytes:     intValue("REGISTRY_HUB_LIMIT_DESCRIPTOR_DATA_BYTES", 16<<20),
		Workers:                 int(intValue("REGISTRY_HUB_WORKERS", 4)),
		PendingTasks:            int(intValue("REGISTRY_HUB_PENDING_TASKS", 1024)),
		MaxPageSize:             int(intValue("REGISTRY_HUB_LIMIT_PAGE_SIZE", 1000)),
		MaxReferrerPages:        int(intValue("REGISTRY_HUB_LIMIT_REFERRER_PAGES", 100)),
		UploadIdleTimeout:       time.Duration(intValue("REGISTRY_HUB_UPLOAD_IDLE_TIMEOUT_SECONDS", 30*60)) * time.Second,
		UploadMaxLifetime:       time.Duration(intValue("REGISTRY_HUB_UPLOAD_MAX_LIFETIME_SECONDS", 24*60*60)) * time.Second,
		GlobalRate:              float64(intValue("REGISTRY_HUB_RATE_GLOBAL", 200)),
		GlobalBurst:             float64(intValue("REGISTRY_HUB_RATE_GLOBAL_BURST", 400)),
		NoAuthRate:              float64(intValue("REGISTRY_HUB_RATE_NO_AUTH", 20)),
		NoAuthBurst:             float64(intValue("REGISTRY_HUB_RATE_NO_AUTH_BURST", 40)),
		RequestTimeout:          time.Duration(intValue("REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS", 300)) * time.Second,
		BlobTransferTimeout:     secondsValue("REGISTRY_HUB_BLOB_TRANSFER_TIMEOUT_SECONDS", 24*60*60),
		UpstreamBodyIdleTimeout: secondsValue("REGISTRY_HUB_UPSTREAM_BODY_IDLE_TIMEOUT_SECONDS", 60),
		DownloadPartialTTL:      secondsValue("REGISTRY_HUB_DOWNLOAD_PARTIAL_TTL_SECONDS", 24*60*60),
		DownloadMaxBytes:        downloadMaxBytes,
	}

	endpoint := value("REGISTRY_HUB_DISTRIBUTION_ENDPOINT", "https://registry:5000")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return cfg, errors.New("REGISTRY_HUB_DISTRIBUTION_ENDPOINT must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "https" && !strings.EqualFold(os.Getenv("REGISTRY_HUB_DISTRIBUTION_ALLOW_HTTP"), "true") {
		return cfg, errors.New("Distribution HTTP requires REGISTRY_HUB_DISTRIBUTION_ALLOW_HTTP=true")
	}
	cfg.DistributionURL = parsed

	var errList error
	if cfg.DistributionRedirectEndpoints, err = loadJSON[[]string]("REGISTRY_HUB_DISTRIBUTION_REDIRECT_ENDPOINTS"); err != nil {
		errList = errors.Join(errList, err)
	}
	clientAuthValue := os.Getenv("REGISTRY_HUB_CLIENT_AUTH_CONFIG")
	clientAuthRequired := strings.EqualFold(os.Getenv("REGISTRY_HUB_CLIENT_AUTH_CONFIG_REQUIRED"), "true")
	if clientAuthValue == "" && clientAuthRequired {
		errList = errors.Join(errList, errors.New("REGISTRY_HUB_CLIENT_AUTH_CONFIG is required"))
	} else if clientAuthValue != "" {
		if cfg.ClientAuth, err = loadJSON[Auth]("REGISTRY_HUB_CLIENT_AUTH_CONFIG"); err != nil {
			errList = errors.Join(errList, err)
		} else if cfg.ClientAuth.Empty() {
			errList = errors.Join(errList, errors.New("REGISTRY_HUB_CLIENT_AUTH_CONFIG must contain credentials"))
		}
	}
	if cfg.AdminAuth, err = loadJSON[Auth]("REGISTRY_HUB_ADMIN_AUTH_CONFIG"); err != nil {
		errList = errors.Join(errList, err)
	}
	if cfg.UpstreamAuth, err = loadJSON[map[string]UpstreamAuth]("REGISTRY_HUB_UPSTREAM_AUTH_CONFIG"); err != nil {
		errList = errors.Join(errList, err)
	}
	if cfg.TransportRules, err = loadJSON[map[string]TransportRule]("REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG"); err != nil {
		errList = errors.Join(errList, err)
	}
	if cfg.Proxy, err = loadJSON[ProxyConfig]("REGISTRY_HUB_PROXY_CONFIG"); err != nil {
		errList = errors.Join(errList, err)
	}
	if cfg.DistributionAuth, err = loadJSON[DistributionAuth]("REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG"); err != nil {
		errList = errors.Join(errList, err)
	}
	if cfg.DistributionAuth.Username == "" && os.Getenv("REGISTRY_HUB_DISTRIBUTION_USERNAME") != "" {
		cfg.DistributionAuth.Username = os.Getenv("REGISTRY_HUB_DISTRIBUTION_USERNAME")
		cfg.DistributionAuth.Password = os.Getenv("REGISTRY_HUB_DISTRIBUTION_PASSWORD")
		cfg.DistributionAuth.Token = os.Getenv("REGISTRY_HUB_INTERNAL_TOKEN")
	}
	if errList != nil {
		return cfg, errList
	}
	cfg.S3 = S3{
		Endpoint:           os.Getenv("REGISTRY_HUB_S3_ENDPOINT"),
		Region:             value("REGISTRY_HUB_S3_REGION", "us-east-1"),
		Bucket:             os.Getenv("REGISTRY_HUB_S3_BUCKET"),
		Prefix:             strings.Trim(os.Getenv("REGISTRY_HUB_S3_PREFIX"), "/"),
		UsePathStyle:       strings.EqualFold(os.Getenv("REGISTRY_HUB_S3_FORCE_PATH_STYLE"), "true"),
		AllowHTTP:          strings.EqualFold(os.Getenv("REGISTRY_HUB_S3_ALLOW_HTTP"), "true"),
		CAFile:             os.Getenv("REGISTRY_HUB_S3_CA_FILE"),
		MultipartThreshold: intValue("REGISTRY_HUB_S3_MULTIPART_THRESHOLD", 64<<20),
		PartSize:           intValue("REGISTRY_HUB_S3_PART_SIZE", 16<<20),
		Timeout:            time.Duration(intValue("REGISTRY_HUB_S3_TIMEOUT_SECONDS", 300)) * time.Second,
		MaxConnections:     int(intValue("REGISTRY_HUB_S3_MAX_CONNECTIONS", 20)),
	}
	if parseErrors != nil {
		return cfg, parseErrors
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}
