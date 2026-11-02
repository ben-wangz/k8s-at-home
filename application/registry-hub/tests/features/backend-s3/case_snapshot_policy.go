package backends3

import "strings"

func transientBackendObject(key string) bool {
	relative := strings.TrimPrefix(key, backendS3Prefix+"/")
	if strings.HasPrefix(relative, "cache/objects/") && strings.HasSuffix(relative, "/representation-index.json") {
		return true
	}
	for _, prefix := range []string{"leases/", "background/v1/jobs/", "background/v1/receipts/", "inventory/v1/changes/"} {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}

func mutableBackendObject(key string) bool {
	relative := strings.TrimPrefix(key, backendS3Prefix+"/")
	switch relative {
	case "system/gc/state.json", "downloads/v1/quota.json", "inventory/v1/checkpoint.json", "inventory/v1/current.json",
		"diagnostics/v1/storage/checkpoint.json", "diagnostics/v1/storage/current.json", "diagnostics/v1/coverage.json":
		return true
	}
	for _, prefix := range []string{
		"background/v1/statistics-checkpoints/", "background/v1/statistics/", "background/v1/maintenance-checkpoints/",
		"background/v1/queue-checkpoints/", "background/v1/source-checkpoints/", "background/v1/status/", "background/v1/scans/",
		"diagnostics/v1/event-budgets/", "diagnostics/v1/workers/", "diagnostics/v1/collected/",
		"inventory/v1/contents/", "inventory/v1/bindings/", "inventory/v1/cache/", "inventory/v1/proofs/",
		"inventory/v1/lifecycle/", "inventory/v1/work/", "inventory/v1/content-work/", "inventory/v1/projection-revisions/",
	} {
		if strings.HasPrefix(relative, prefix) {
			return true
		}
	}
	return false
}
