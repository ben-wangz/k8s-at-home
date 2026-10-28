package inventory

import (
	"fmt"
	"net/url"
)

func matchesImage(i Image, f url.Values) bool {
	for key, value := range map[string]string{"authority": i.Authority, "repository": i.Repository, "reference": i.Reference, "digest": i.ResolvedDigest, "category": i.Category, "cacheState": i.Cache.State, "executionState": i.Solidification.ExecutionState} {
		if f.Get(key) != "" && f.Get(key) != value {
			return false
		}
	}
	if p := f.Get("platform"); p != "" {
		if i.Platform == nil {
			return false
		}
		name := i.Platform.OS + "/" + i.Platform.Architecture
		if i.Platform.Variant != "" {
			name += "/" + i.Platform.Variant
		}
		if p != name {
			return false
		}
	}
	return true
}

func matchesObject(o Object, f url.Values) bool {
	for key, value := range map[string]string{"authority": o.Authority, "repository": o.Repository, "digest": o.Digest, "cacheState": o.CacheState} {
		if f.Get(key) != "" && f.Get(key) != value {
			return false
		}
	}
	if value := f.Get("associated"); value != "" && value != fmt.Sprint(o.Associated) {
		return false
	}
	return f.Get("cleanupPending") == "" || f.Get("cleanupPending") == fmt.Sprint(o.CleanupPending)
}
