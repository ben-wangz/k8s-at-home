package app

import (
	"fmt"
	"registry-hub/internal/diagnostics"
)

var storageDimensions = []string{"category", "repository", "image", "platform"}

func usageName(usage storageUsage, bit int) string {
	return []string{usage.Category, usage.Repository, usage.ImageID, usage.Platform}[bit]
}

func storageUsageKey(usage storageUsage) string {
	if usage.Mask == 0 {
		return usage.Domain + "\x00" + usage.Dimension + "\x00" + usage.Name
	}
	for bit, dimension := range storageDimensions {
		if usage.Mask == 1<<bit {
			return usage.Domain + "\x00" + dimension + "\x00" + usageName(usage, bit)
		}
	}
	return usage.Domain + "\x00joint\x00" + diagnostics.ID(fmt.Sprint(usage.Mask), usage.Category, usage.Repository, usage.ImageID, usage.Platform)
}

func storageUsageGroups(object storageObject) map[string]storageUsage {
	associations := object.Associations
	if len(associations) == 0 {
		associations = []storageAssociation{{Authority: object.Authority, Repository: object.Repository, ImageID: object.ImageID}}
	}
	groups := map[string]storageUsage{}
	for _, association := range associations {
		repository := association.Authority + "/" + association.Repository
		if association.Repository == "" {
			repository = "unknown"
		}
		platform := ""
		if p := association.Platform; p != nil {
			platform = p.OS + "/" + p.Architecture
			if p.Variant != "" {
				platform += "/" + p.Variant
			}
		}
		addStorageUsageTuple(groups, object.Domain, []string{object.Category, repository, association.ImageID, platform})
	}
	return groups
}

func addStorageUsageTuple(groups map[string]storageUsage, domain string, names []string) {
	for mask := uint8(1); mask < 16; mask++ {
		usage := storageUsage{Domain: domain, Mask: mask}
		values := []*string{&usage.Category, &usage.Repository, &usage.ImageID, &usage.Platform}
		valid := true
		for bit, name := range names {
			if mask&(1<<bit) != 0 {
				*values[bit] = name
				valid = valid && name != ""
			}
			if mask == 1<<bit {
				usage.Dimension, usage.Name = storageDimensions[bit], name
			}
		}
		if valid {
			groups[storageUsageKey(usage)] = usage
		}
	}
}
