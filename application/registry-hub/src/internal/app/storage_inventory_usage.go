package app

func addStorageAttributionUsage(state *storageCollection, object storageObject) {
	if object.AssociationsTruncated {
		state.Snapshot.AttributionCoverage = "partial; captured inventory associations truncated"
		state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "association_limit")
	}
	for key, group := range storageUsageGroups(object) {
		usage, exists := state.Usage[key]
		if !exists && state.Snapshot.UsageVersion == 0 && len(state.Usage) >= 4096 {
			state.Snapshot.Reasons = appendUniqueReason(state.Snapshot.Reasons, "usage_group_limit")
			continue
		}
		if !exists {
			usage = group
		}
		if usage.Mask&1 != 0 {
			usage.LogicalUnknown = true
		}
		usage.Attribution = "physical object union per group; image/repository/platform groups overlap; sharing is relative to captured inventory canonical content IDs"
		usage.ObjectBytes += object.ObjectBytes
		usage.Objects++
		if object.Category == "cache_payloads" || object.Category == "registry_payloads" {
			logical := object.ObjectBytes
			if usage.ResidentPayloadBytes != nil {
				logical += *usage.ResidentPayloadBytes
			}
			usage.ResidentPayloadBytes = &logical
			if object.Shared == nil {
				usage.UnknownAttributionBytes += object.ObjectBytes
			} else {
				exclusive, shared := int64(0), int64(0)
				if usage.ExclusiveBytes != nil {
					exclusive = *usage.ExclusiveBytes
				}
				if usage.SharedBytes != nil {
					shared = *usage.SharedBytes
				}
				if *object.Shared {
					shared += object.ObjectBytes
				} else {
					exclusive += object.ObjectBytes
				}
				usage.ExclusiveBytes, usage.SharedBytes = &exclusive, &shared
			}
		}
		state.Usage[key] = usage
	}
}
