package app

import "registry-hub/internal/inventory"

func summarizeInventoryObjects(objects []inventory.Object, complete bool) (inventory.CacheState, inventory.Availability, bool) {
	cache := inventory.CacheState{State: "none"}
	availability := inventory.Availability{State: "partial"}
	expected, expectedBytes, missing, missingBytes := int64(len(objects)), int64(0), int64(0), int64(0)
	cacheUnknown, localUnknown, anchored := !complete, !complete, complete
	for _, object := range objects {
		expectedBytes += object.Size
		if object.CacheState == "complete" {
			cache.Objects.Cached++
			cache.LogicalBytes.Cached += object.Size
		}
		if object.CacheState == "unknown" {
			cacheUnknown = true
		}
		if object.DistributionVerified {
			cache.Objects.DistributionVerified++
			cache.LogicalBytes.DistributionVerified += object.Size
		}
		if object.CleanupPending && object.CacheState == "complete" {
			cache.Objects.CleanupPending++
			cache.LogicalBytes.CleanupPending += object.Size
		}
		if object.CacheState != "complete" && !object.DistributionVerified {
			missing++
			missingBytes += object.Size
		}
		if !object.Anchored {
			anchored = false
		}
		for _, reason := range object.Reasons {
			if reason == "local_evidence_stale" || reason == "local_evidence_invalid" || reason == "local_evidence_unavailable" || reason == "local_read_error" || reason == "storage_read_error" {
				localUnknown = true
			}
		}
	}
	if complete {
		cache.Objects.Expected = &expected
		cache.LogicalBytes.Expected = &expectedBytes
		if !localUnknown && !cacheUnknown {
			cache.Objects.Missing = &missing
			cache.LogicalBytes.Missing = &missingBytes
		}
	}
	if cacheUnknown {
		cache.State = "unknown"
	} else if cache.Objects.Cached == expected {
		cache.State = "complete"
	} else if cache.Objects.Cached > 0 {
		cache.State = "partial"
	}
	if complete && missing == 0 {
		availability.State = "complete"
	} else if localUnknown || cacheUnknown {
		availability.State = "unknown"
	}
	return cache, availability, anchored
}
