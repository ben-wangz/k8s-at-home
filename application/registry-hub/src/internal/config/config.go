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
