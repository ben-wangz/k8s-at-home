package gc

import "testing"

func TestCacheCleanupRequiresLiveEvidenceForTheTargetBlob(t *testing.T) {
	for _, test := range []struct {
		name, consistency, digest, role, state string
		readErrors                             map[string]any
		missing, wantError                     bool
	}{
		{name: "automatic cleanup already removed target", consistency: "live", digest: "target", role: "blob", state: "missing", missing: true},
		{name: "cache remains", consistency: "live", digest: "target", role: "blob", state: "complete"},
		{name: "unknown is not a miss", consistency: "live", digest: "target", role: "blob", state: "unknown"},
		{name: "snapshot cannot prove current state", consistency: "snapshot", digest: "target", role: "blob", state: "missing", wantError: true},
		{name: "other blob is not evidence", consistency: "live", digest: "other", role: "blob", state: "missing", wantError: true},
		{name: "manifest is not target blob", consistency: "live", digest: "target", role: "manifest", state: "missing", wantError: true},
		{name: "read failure cannot prove a miss", consistency: "live", digest: "target", role: "blob", state: "missing", readErrors: map[string]any{"cache": "unavailable"}, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := map[string]any{"consistency": test.consistency, "items": []any{map[string]any{
				"digest": test.digest, "role": test.role, "cacheState": test.state, "readErrors": test.readErrors,
			}}}
			missing, err := cacheDependencyMissing(page, "target")
			if missing != test.missing || (err != nil) != test.wantError {
				t.Fatalf("missing=%v err=%v; want missing=%v error=%v", missing, err, test.missing, test.wantError)
			}
		})
	}
}
