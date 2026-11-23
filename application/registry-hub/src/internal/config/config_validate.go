package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"time"
)

func (c Config) Validate() error {
	diagnostics := c.Diagnostics
	if diagnostics.OrdinaryEventFlushInterval != 0 && (diagnostics.OrdinaryEventFlushInterval < time.Second || diagnostics.OrdinaryEventFlushInterval > time.Hour) {
		return errors.New("REGISTRY_HUB_DIAGNOSTIC_EVENT_FLUSH_SECONDS must be between 1 and 3600 seconds")
	}
	if diagnostics.CriticalEventFlushInterval != 0 && (diagnostics.CriticalEventFlushInterval < time.Second || diagnostics.CriticalEventFlushInterval > time.Minute) {
		return errors.New("REGISTRY_HUB_DIAGNOSTIC_EVENT_CRITICAL_FLUSH_SECONDS must be between 1 and 60 seconds")
	}
	for _, limit := range []struct {
		name  string
		value int64
	}{
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_BYTES_LIMIT", diagnostics.EventBatchDailyBytes},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_COUNT_LIMIT", diagnostics.EventBatchDailyCount},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_BYTES_LIMIT", diagnostics.EventBatchGlobalBytes},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_COUNT_LIMIT", diagnostics.EventBatchGlobalCount},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_BYTES", diagnostics.EventBatchDailyCriticalBytes},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_COUNT", diagnostics.EventBatchDailyCriticalCount},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_BYTES", diagnostics.EventBatchGlobalCriticalBytes},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_COUNT", diagnostics.EventBatchGlobalCriticalCount},
	} {
		if limit.value <= 0 {
			return fmt.Errorf("%s must be greater than zero", limit.name)
		}
	}
	if diagnostics.EventBatchDailyCriticalBytes > diagnostics.EventBatchDailyBytes || diagnostics.EventBatchDailyCriticalCount > diagnostics.EventBatchDailyCount ||
		diagnostics.EventBatchGlobalCriticalBytes > diagnostics.EventBatchGlobalBytes || diagnostics.EventBatchGlobalCriticalCount > diagnostics.EventBatchGlobalCount {
		return errors.New("diagnostic event batch critical reserves must fit within their daily and global limits")
	}
	for _, retention := range []struct {
		name  string
		value time.Duration
	}{
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_RETENTION_SECONDS", diagnostics.EventRetention},
		{"REGISTRY_HUB_DIAGNOSTIC_EVENT_BUDGET_RETENTION_SECONDS", diagnostics.EventBudgetRetention},
		{"REGISTRY_HUB_DIAGNOSTIC_COLLECTION_RETENTION_SECONDS", diagnostics.CollectedReceiptRetention},
		{"REGISTRY_HUB_RETRY_REQUEST_RETENTION_SECONDS", diagnostics.RetryRequestRetention},
		{"REGISTRY_HUB_DIAGNOSTIC_TRANSFER_RETENTION_SECONDS", diagnostics.TransferObservationRetention},
		{"REGISTRY_HUB_TRANSFER_EXPIRY_RETENTION_SECONDS", diagnostics.TransferExpiryRetention},
		{"REGISTRY_HUB_TERMINAL_HISTORY_RETENTION_SECONDS", diagnostics.TerminalHistoryRetention},
		{"REGISTRY_HUB_FULL_WORK_RECEIPT_RETENTION_SECONDS", diagnostics.FullWorkReceiptRetention},
		{"REGISTRY_HUB_HISTORY_CLEANUP_PREVIEW_TTL_SECONDS", diagnostics.HistoryCleanupPreviewTTL},
		{"REGISTRY_HUB_HISTORY_CLEANUP_RESULT_RETENTION_SECONDS", diagnostics.HistoryCleanupResultRetention},
	} {
		if retention.value != 0 && (retention.value < time.Second || retention.value > 365*24*time.Hour) {
			return fmt.Errorf("%s must be between 1 and 31536000 seconds", retention.name)
		}
	}
	if diagnostics.RetentionSweepInterval != 0 && (diagnostics.RetentionSweepInterval < time.Second || diagnostics.RetentionSweepInterval > 15*time.Minute) {
		return errors.New("REGISTRY_HUB_HISTORY_RETENTION_SWEEP_INTERVAL_SECONDS must be between 1 and 900 seconds")
	}
	if diagnostics.HistorySweepInterval != 0 && (diagnostics.HistorySweepInterval < time.Second || diagnostics.HistorySweepInterval > 15*time.Minute) {
		return errors.New("REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS must be between 1 and 900 seconds")
	}
	if diagnostics.HistorySweepMaxInterval != 0 && (diagnostics.HistorySweepMaxInterval < time.Second || diagnostics.HistorySweepMaxInterval > 15*time.Minute) {
		return errors.New("REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS must be between 1 and 900 seconds")
	}
	if diagnostics.HistorySweepInterval != 0 && diagnostics.HistorySweepMaxInterval != 0 && diagnostics.HistorySweepMaxInterval < diagnostics.HistorySweepInterval {
		return errors.New("REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS must be at least REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS")
	}
	if diagnostics.CollectedReceiptRetention != 0 && diagnostics.EventRetention != 0 && diagnostics.CollectedReceiptRetention < diagnostics.EventRetention {
		return errors.New("diagnostic collection retention must be at least diagnostic event retention")
	}
	if diagnostics.EventBudgetRetention != 0 && diagnostics.EventRetention != 0 && diagnostics.EventBudgetRetention < diagnostics.EventRetention {
		return errors.New("diagnostic event budget retention must be at least diagnostic event retention")
	}
	if diagnostics.FullWorkReceiptRetention != 0 && diagnostics.TerminalHistoryRetention != 0 && diagnostics.FullWorkReceiptRetention < diagnostics.TerminalHistoryRetention {
		return errors.New("full work receipt retention must be at least terminal history retention")
	}
	if diagnostics.CollectedReceiptRetention != 0 && diagnostics.FullWorkReceiptRetention != 0 && diagnostics.CollectedReceiptRetention < diagnostics.FullWorkReceiptRetention {
		return errors.New("diagnostic collection retention must be at least full work receipt retention")
	}
	if diagnostics.TransferObservationRetention != 0 && diagnostics.TerminalHistoryRetention != 0 && diagnostics.TransferObservationRetention < diagnostics.TerminalHistoryRetention {
		return errors.New("transfer observation retention must be at least terminal history retention")
	}
	if diagnostics.TransferExpiryRetention != 0 && diagnostics.TerminalHistoryRetention != 0 && diagnostics.TransferExpiryRetention < diagnostics.TerminalHistoryRetention {
		return errors.New("transfer expiry retention must be at least terminal history retention")
	}
	if c.Diagnostics.FullWorkReceiptCleanupEnabled && !c.Diagnostics.HistoryProjectionReadsEnabled {
		return errors.New("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_ENABLED requires REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED=true")
	}
	if c.Diagnostics.FullWorkReceiptCleanupPreview && !c.Diagnostics.HistoryProjectionReadsEnabled {
		return errors.New("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_PREVIEW requires REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED=true")
	}
	if c.Diagnostics.FullWorkReceiptCleanupPreview && c.Diagnostics.FullWorkReceiptCleanupEnabled {
		return errors.New("REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_PREVIEW and REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_ENABLED cannot both be true")
	}
	if err := c.RegistryInspection.validate(c); err != nil {
		return err
	}
	if err := c.Inventory.validateOptional(); err != nil {
		return err
	}
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
	if c.S3.ReplayMemoryBytes <= 0 || c.S3.ReplayMaxMemoryBytes <= 0 || c.S3.ReplayMaxDiskBytes <= 0 || c.S3.ReplayMemoryBytes > c.S3.ReplayMaxMemoryBytes || !filepath.IsAbs(c.S3.ReplayDirectory) {
		return errors.New("S3 replay limits must be positive, per-source memory must not exceed the global memory budget, and the replay directory must be absolute")
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
	if c.Limits.ManifestBytes < 1 || c.Limits.UploadBytes < 1 || c.Limits.ObjectBytes < 1 || c.Limits.ObjectBytes > math.MaxInt64/2 || c.Limits.ClosureObjects < 1 || c.Limits.ClosureBytes < 1 || c.Limits.ClosureDepth < 1 || c.Limits.MaxDescriptors < 1 || c.Limits.DescriptorDataBytes < 1 || c.Limits.Workers < 1 || c.Limits.Workers > 4 || c.Limits.PendingTasks < 1 || c.Limits.MaxPageSize < 1 || c.Limits.MaxReferrerPages < 1 || c.Limits.UploadIdleTimeout <= 0 || c.Limits.UploadMaxLifetime <= 0 || c.Limits.GlobalRate <= 0 || c.Limits.GlobalBurst < 1 || c.Limits.NoAuthRate <= 0 || c.Limits.NoAuthBurst < 1 || c.Limits.RequestTimeout <= 0 || c.Limits.BlobTransferTimeout <= 0 || c.Limits.UpstreamBodyIdleTimeout <= 0 || c.Limits.DownloadFinalizeTimeout <= 0 || c.Limits.DownloadPartialTTL <= 0 || c.Limits.DownloadMaxBytes <= 0 {
		return errors.New("resource limits must be positive")
	}
	if c.Solidification.IdleTimeout <= 0 || c.Solidification.MaxWaitingRoots < 1 || c.Solidification.MaxBlobIntents < 1 || c.Solidification.MaxProtectionSessions < 1 || c.Solidification.MaxProtectionSessions > 4096 || c.Solidification.MaxProtectedCacheBytes < 1 || c.Solidification.MaxUnanchoredDistributionBytes < 1 {
		return errors.New("solidification limits must be positive, with at most 4096 protection sessions")
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
