package config

import (
	"net/url"
	"time"
)

type Credential struct {
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Token       string   `json:"token"`
	Permissions []string `json:"permissions"`
}

type Auth struct {
	Basic  []Credential `json:"basic"`
	Bearer []Credential `json:"bearer"`
}

func (a Auth) Empty() bool { return len(a.Basic) == 0 && len(a.Bearer) == 0 }

type UpstreamAuth struct {
	Type     string `json:"type"`
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type TransportRule struct {
	AllowHTTP bool     `json:"allowHTTP"`
	CAFile    string   `json:"caFile"`
	Ports     []uint16 `json:"ports"`
}

type ProxyConfig struct {
	Default     string            `json:"default"`
	Authorities map[string]string `json:"authorities"`
}

type DistributionAuth struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

type ImagePullSecretReference struct {
	Name string `json:"name"`
}

type S3 struct {
	AccessKeyID          string `json:"-"`
	SecretAccessKey      string `json:"-"`
	Endpoint             string
	Region               string
	Bucket               string
	Prefix               string
	UsePathStyle         bool
	AllowHTTP            bool
	CAFile               string
	MultipartThreshold   int64
	PartSize             int64
	Timeout              time.Duration
	MaxConnections       int
	ReplayMemoryBytes    int64
	ReplayMaxMemoryBytes int64
	ReplayMaxDiskBytes   int64
	ReplayDirectory      string
}

type Limits struct {
	ManifestBytes           int64
	UploadBytes             int64
	ObjectBytes             int64
	ClosureObjects          int
	ClosureBytes            int64
	ClosureDepth            int
	MaxDescriptors          int
	DescriptorDataBytes     int64
	Workers                 int
	PendingTasks            int
	MaxPageSize             int
	MaxReferrerPages        int
	UploadIdleTimeout       time.Duration
	UploadMaxLifetime       time.Duration
	GlobalRate              float64
	GlobalBurst             float64
	NoAuthRate              float64
	NoAuthBurst             float64
	RequestTimeout          time.Duration
	BlobTransferTimeout     time.Duration
	UpstreamBodyIdleTimeout time.Duration
	DownloadFinalizeTimeout time.Duration
	DownloadPartialTTL      time.Duration
	DownloadMaxBytes        int64
}

type Solidification struct {
	IdleTimeout                    time.Duration
	MaxWaitingRoots                int
	MaxBlobIntents                 int
	MaxProtectionSessions          int
	MaxProtectedCacheBytes         int64
	MaxUnanchoredDistributionBytes int64
}

type Diagnostics struct {
	OrdinaryEventFlushInterval    time.Duration
	CriticalEventFlushInterval    time.Duration
	EventRetention                time.Duration
	EventBudgetRetention          time.Duration
	CollectedReceiptRetention     time.Duration
	RetryRequestRetention         time.Duration
	TransferObservationRetention  time.Duration
	TransferExpiryRetention       time.Duration
	TerminalHistoryRetention      time.Duration
	FullWorkReceiptRetention      time.Duration
	HistoryCleanupPreviewTTL      time.Duration
	HistoryCleanupResultRetention time.Duration
	HistorySweepInterval          time.Duration
	HistorySweepMaxInterval       time.Duration
	RetentionSweepInterval        time.Duration
	HistoryProjectionReadsEnabled bool
	FullWorkReceiptCleanupPreview bool
	FullWorkReceiptCleanupEnabled bool
}

func DefaultDiagnostics() Diagnostics {
	return Diagnostics{
		OrdinaryEventFlushInterval:    time.Minute,
		CriticalEventFlushInterval:    5 * time.Second,
		EventRetention:                7 * 24 * time.Hour,
		EventBudgetRetention:          8 * 24 * time.Hour,
		CollectedReceiptRetention:     30 * 24 * time.Hour,
		RetryRequestRetention:         30 * 24 * time.Hour,
		TransferObservationRetention:  30 * 24 * time.Hour,
		TransferExpiryRetention:       30 * 24 * time.Hour,
		TerminalHistoryRetention:      48 * time.Hour,
		FullWorkReceiptRetention:      3 * 24 * time.Hour,
		HistoryCleanupPreviewTTL:      10 * time.Minute,
		HistoryCleanupResultRetention: 7 * 24 * time.Hour,
		HistorySweepInterval:          15 * time.Second,
		HistorySweepMaxInterval:       5 * time.Minute,
		RetentionSweepInterval:        15 * time.Second,
	}
}

func (c Diagnostics) WithDefaults() Diagnostics {
	defaults := DefaultDiagnostics()
	if c.OrdinaryEventFlushInterval <= 0 {
		c.OrdinaryEventFlushInterval = defaults.OrdinaryEventFlushInterval
	}
	if c.CriticalEventFlushInterval <= 0 {
		c.CriticalEventFlushInterval = defaults.CriticalEventFlushInterval
	}
	if c.EventRetention <= 0 {
		c.EventRetention = defaults.EventRetention
	}
	if c.CollectedReceiptRetention <= 0 {
		c.CollectedReceiptRetention = defaults.CollectedReceiptRetention
	}
	if c.EventBudgetRetention <= 0 {
		c.EventBudgetRetention = defaults.EventBudgetRetention
	}
	if c.RetryRequestRetention <= 0 {
		c.RetryRequestRetention = defaults.RetryRequestRetention
	}
	if c.TransferObservationRetention <= 0 {
		c.TransferObservationRetention = defaults.TransferObservationRetention
	}
	if c.TransferExpiryRetention <= 0 {
		c.TransferExpiryRetention = defaults.TransferExpiryRetention
	}
	if c.TerminalHistoryRetention <= 0 {
		c.TerminalHistoryRetention = defaults.TerminalHistoryRetention
	}
	if c.FullWorkReceiptRetention <= 0 {
		c.FullWorkReceiptRetention = defaults.FullWorkReceiptRetention
	}
	if c.HistoryCleanupPreviewTTL <= 0 {
		c.HistoryCleanupPreviewTTL = defaults.HistoryCleanupPreviewTTL
	}
	if c.HistoryCleanupResultRetention <= 0 {
		c.HistoryCleanupResultRetention = defaults.HistoryCleanupResultRetention
	}
	if c.HistorySweepInterval <= 0 {
		c.HistorySweepInterval = defaults.HistorySweepInterval
	}
	if c.HistorySweepMaxInterval <= 0 {
		c.HistorySweepMaxInterval = defaults.HistorySweepMaxInterval
	}
	if c.RetentionSweepInterval <= 0 {
		c.RetentionSweepInterval = defaults.RetentionSweepInterval
	}
	return c
}

type Config struct {
	ClientAddr                    string
	AdminAddr                     string
	ClientAuth                    Auth
	AdminAuth                     Auth
	DistributionURL               *url.URL
	DistributionAuth              DistributionAuth
	DistributionCA                string
	DistributionRedirectEndpoints []string
	StorageBackend                string
	FilesystemRoot                string
	S3                            S3
	UpstreamAuth                  map[string]UpstreamAuth
	TransportRules                map[string]TransportRule
	Proxy                         ProxyConfig
	Limits                        Limits
	Solidification                Solidification
	Diagnostics                   Diagnostics
	Inventory                     Inventory
	RegistryInspection            RegistryInspection
	CatalogEnabled                bool
	InternalToken                 string
	GCMode                        string
	GCExecutable                  string
	GCConfig                      string
	GCLifecycleExecutable         string
	GCLifecycleConfig             string
	GCImage                       string
	GCImagePullSecrets            []ImagePullSecretReference
	GCConfigMap                   string
	GCConfigDigest                string
	GCEnvSecret                   string
	GCConfigKey                   string
	GCStoragePVC                  string
	GCServiceAccount              string
	GCNamespace                   string
	GCUniqueWriterConfirmed       bool
	GCUploadPurgingDisabled       bool
}
