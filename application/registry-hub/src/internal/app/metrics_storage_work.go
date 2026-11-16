package app

import (
	"context"
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"registry-hub/internal/inventory"
	"registry-hub/internal/storage"
)

type storagePurposeKey struct{}
type storageReasonKey struct{}

func withStorageReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, storageReasonKey{}, boundedStorageReason(reason))
}

func attributedStorageContext(ctx context.Context, operation, key string) context.Context {
	return storage.WithCallAttribution(ctx, storage.CallAttribution{
		Operation: operation,
		KeyClass:  storageKeyClass(key),
		Reason:    storageReasonLabel(ctx),
	})
}

func storageReasonLabel(ctx context.Context) string {
	if ctx == nil {
		return "unspecified"
	}
	reason, _ := ctx.Value(storageReasonKey{}).(string)
	if reason == "" && ctx.Value(requestMetaKey{}) != nil {
		reason = "request"
	}
	return boundedStorageReason(reason)
}

func storageWorkLabels(ctx context.Context) (string, string) {
	kind, purpose := "service", "work"
	if ctx.Value(requestMetaKey{}) != nil {
		kind = "request"
	}
	if identity, ok := ctx.Value(diagnosticContextKey{}).(backgroundDiagnosticContext); ok {
		kind = identity.Kind
		if !slices.Contains(backgroundKinds, kind) && !slices.Contains(backgroundScanKinds, kind) {
			switch kind {
			case "activation_events", "inventory_builder", "statistics_tasks", "statistics_cache", "statistics_intents", "statistics_protection", "storage_probe", "registry_probe", "index_repair", "solidification", "protection_recovery", "diagnostic_events":
			default:
				kind = "other"
			}
		}
		switch identity.Scope {
		case "checkpoint":
			purpose = "checkpoint"
		case "lease_acquisition", "lease_renewal", "lease_release":
			purpose = "lease"
		}
	}
	if value, _ := ctx.Value(storagePurposeKey{}).(string); value == "queue_poll" {
		purpose = value
	}
	return kind, purpose
}

func (m *metrics) recordStorageWork(ctx context.Context, operation, result string, elapsed time.Duration) {
	if m == nil || m.storageWorkOps == nil {
		return
	}
	kind, purpose := storageWorkLabels(ctx)
	attribution := storage.CallAttributionFromContext(ctx)
	labels := []string{kind, purpose, boundedStorageOperation(operation), boundedStorageKeyClass(attribution.KeyClass), boundedStorageReason(attribution.Reason), result}
	m.storageWorkOps.WithLabelValues(labels...).Inc()
	m.storageWorkLatency.WithLabelValues(labels...).Observe(elapsed.Seconds())
}

func (m *metrics) recordBackgroundScanRound(kind string, complete bool) {
	if m == nil || m.backgroundScanRounds == nil {
		return
	}
	result := "incomplete"
	if complete {
		result = "complete"
	}
	m.backgroundScanRounds.WithLabelValues(boundedBackgroundScanKind(kind), result).Inc()
}

func boundedBackgroundScanKind(kind string) string {
	if slices.Contains(backgroundScanKinds, kind) {
		return kind
	}
	switch kind {
	case "inventory":
		return kind
	case "statistics_tasks", "statistics_cache", "statistics_intents", "statistics_protection":
		return kind
	default:
		return "other"
	}
}

func (m *metrics) recordInventorySnapshotWrite(ctx context.Context, key string, bytes int64) {
	if m == nil || m.inventorySnapshotWrites == nil || storageKeyClass(key) != "inventory_snapshot" {
		return
	}
	kind, purpose := storageWorkLabels(ctx)
	labels := []string{kind, purpose, storageReasonLabel(ctx)}
	m.inventorySnapshotWrites.WithLabelValues(labels...).Inc()
	if bytes > 0 {
		m.inventorySnapshotWriteBytes.WithLabelValues(labels...).Add(float64(bytes))
	}
}

func (m *metrics) recordS3HTTPAttempt(ctx context.Context, attempt storage.HTTPAttempt) {
	if m == nil || m.s3HTTPAttempts == nil {
		return
	}
	kind, purpose := storageWorkLabels(ctx)
	operation := attempt.Operation
	if operation == "" {
		operation = boundedHTTPMethod(attempt.Method)
	} else {
		operation = boundedStorageOperation(operation)
	}
	labels := []string{
		boundedStorageDomain(attempt.Domain), kind, purpose, operation,
		boundedStorageKeyClass(attempt.KeyClass), boundedStorageReason(attempt.Reason), storageHTTPResult(attempt),
	}
	m.s3HTTPAttempts.WithLabelValues(labels...).Inc()
	elapsed := attempt.FinishedAt.Sub(attempt.StartedAt).Seconds()
	if elapsed < 0 {
		elapsed = 0
	}
	m.s3HTTPLatency.WithLabelValues(labels...).Observe(elapsed)
}

func storageHTTPResult(attempt storage.HTTPAttempt) string {
	if attempt.Error != nil {
		if errors.Is(attempt.Error, storage.ErrTimeout) || errors.Is(attempt.Error, context.DeadlineExceeded) {
			return "timeout"
		}
		var networkError net.Error
		if errors.As(attempt.Error, &networkError) && networkError.Timeout() {
			return "timeout"
		}
		return "transport_error"
	}
	switch {
	case attempt.Status == 404:
		return "not_found"
	case attempt.Status == 409 || attempt.Status == 412:
		return "condition_failed"
	case attempt.Status >= 500:
		return "server_error"
	case attempt.Status >= 400:
		return "client_error"
	case attempt.Status >= 200 && attempt.Status < 400:
		return "success"
	default:
		return "other"
	}
}

func boundedStorageKeyClass(value string) string {
	switch value {
	case "inventory", "inventory_snapshot", "inventory_checkpoint", "inventory_pointer", "task_state", "cache", "lease", "registry", "diagnostics", "queue", "probe", "system", "orphan_cleanup", "other":
		return value
	default:
		return "other"
	}
}

func boundedStorageReason(value string) string {
	switch value {
	case "inventory_refresh", "inventory_full_reconcile", "inventory_snapshot_build", "inventory_maintenance", "health_probe", "storage_inventory", "request":
		return value
	default:
		return "unspecified"
	}
}

func boundedStorageDomain(value string) string {
	if value == "state" || value == "registry" {
		return value
	}
	return "other"
}

func boundedStorageOperation(value string) string {
	switch value {
	case "get", "head", "put", "put_verified", "delete", "delete_if_match", "list", "cleanup_orphans", "close":
		return value
	default:
		return "other"
	}
}

func boundedHTTPMethod(value string) string {
	switch strings.ToUpper(value) {
	case "GET", "HEAD", "PUT", "POST", "DELETE":
		return strings.ToLower(value)
	default:
		return "other"
	}
}

func storageKeyClass(key string) string {
	switch {
	case strings.HasPrefix(key, inventory.Prefix+"snapshots/"):
		return "inventory_snapshot"
	case key == inventory.Prefix+"checkpoint.json":
		return "inventory_checkpoint"
	case key == inventory.Prefix+"current.json":
		return "inventory_pointer"
	case strings.HasPrefix(key, "inventory/"):
		return "inventory"
	case strings.HasPrefix(key, "tasks/"), strings.HasPrefix(key, "solidification/"):
		return "task_state"
	case strings.HasPrefix(key, "cache/"):
		return "cache"
	case strings.HasPrefix(key, "leases/"):
		return "lease"
	case strings.HasPrefix(key, "distribution/"):
		return "registry"
	case strings.HasPrefix(key, "diagnostics/"):
		return "diagnostics"
	case strings.HasPrefix(key, "background/"), strings.HasPrefix(key, "workqueue/"):
		return "queue"
	case strings.HasPrefix(key, "system/probes/"):
		return "probe"
	case strings.HasPrefix(key, "system/"):
		return "system"
	case key == "":
		return "orphan_cleanup"
	default:
		return "other"
	}
}
