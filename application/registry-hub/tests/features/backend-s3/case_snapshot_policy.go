package backends3

import "strings"

func transientBackendObject(key string) bool {
	relative := strings.TrimPrefix(key, backendS3Prefix+"/")
	if strings.HasPrefix(relative, "cache/objects/") && strings.HasSuffix(relative, "/representation-index.json") {
		return true
	}
	for _, prefix := range []string{"leases/", "tasks/wake/", "background/v1/jobs/", "background/v1/receipts/", "inventory/v1/changes/"} {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}

func mutableBackendObject(key string) bool {
	relative := strings.TrimPrefix(key, backendS3Prefix+"/")
	switch relative {
	case "system/gc/state.json", "system/recovery/protection.json", "downloads/v1/quota.json", "inventory/v1/checkpoint.json", "inventory/v1/current.json", "inventory/v1/retention.json", "inventory/v1/work-retention.json",
		"background/v1/quota-signal.json", "diagnostics/v1/storage/checkpoint.json", "diagnostics/v1/storage/current.json", "diagnostics/v1/coverage.json", "inventory/v1/fragment-retention.json":
		return true
	}
	for _, prefix := range []string{
		"tasks/state/",
		"solidification/blobs/intents/",
		"system/solidification/sessions/",
		"background/v1/statistics-checkpoints/", "background/v1/statistics/", "background/v1/maintenance-checkpoints/",
		"background/v1/queue-checkpoints/", "background/v1/source-checkpoints/", "background/v1/status/", "background/v1/scans/",
		"diagnostics/v1/event-budgets/", "diagnostics/v1/workers/", "diagnostics/v1/collected/",
		"inventory/v1/contents/", "inventory/v1/bindings/", "inventory/v1/cache/", "inventory/v1/proofs/",
		"inventory/v1/lifecycle/", "inventory/v1/work/", "inventory/v1/content-work/", "inventory/v1/latest-work/", "inventory/v1/projection-revisions/",
		"inventory/v1/fragments/image/", "inventory/v1/fragments/content/",
	} {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}

func mutableUnpublishedAssociation(key string, before map[string]string) bool {
	prefix := backendS3Prefix + "/diagnostics/v1/storage/snapshots/"
	relative, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return false
	}
	snapshot, suffix, ok := strings.Cut(relative, "/")
	if !ok || snapshot == "" {
		return false
	}
	if _, published := before[prefix+snapshot+"/manifest.json"]; published {
		return false
	}
	parts := strings.Split(suffix, "/")
	return len(parts) == 3 && parts[0] == "associations" && (parts[1] == "state" || parts[1] == "registry") && strings.HasSuffix(parts[2], ".json")
}
