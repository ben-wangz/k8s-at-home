package app

import (
	"context"
	"slices"
	"time"
)

type storagePurposeKey struct{}

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
	m.storageWorkOps.WithLabelValues(kind, purpose, operation, result).Inc()
	m.storageWorkLatency.WithLabelValues(kind, purpose, operation, result).Observe(elapsed.Seconds())
}
