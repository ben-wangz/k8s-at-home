package inventory

import "testing"

func TestClassificationPriority(t *testing.T) {
	no, yes := false, true
	cases := []struct {
		name, cache, availability string
		started                   *bool
		works                     []Work
		closure, anchored, tag    bool
		want                      string
	}{
		{"queued cache", "complete", "complete", &no, []Work{{Status: "pending"}}, true, false, false, "cached_not_started"},
		{"active wins failure", "partial", "partial", &yes, []Work{{Status: "failed"}, {Status: "retry_wait"}}, true, false, false, "solidifying"},
		{"old failure ignored", "none", "complete", &yes, []Work{{Status: "solidified"}, {Status: "failed"}}, true, false, false, "not_scheduled"},
		{"failed", "complete", "complete", &no, []Work{{Status: "failed"}}, true, false, false, "failed"},
		{"cancelled", "partial", "partial", &yes, []Work{{Status: "cancelled"}}, true, false, false, "cancelled"},
		{"legacy cache", "complete", "complete", nil, nil, true, false, false, "cached_start_unknown"},
		{"missing layer", "partial", "partial", &no, nil, true, false, false, "waiting_cache"},
		{"unknown closure", "unknown", "unknown", nil, nil, false, false, false, "unknown"},
		{"anchored tag", "none", "complete", &yes, nil, true, true, true, "solidified"},
		{"unpublished tag", "none", "complete", &yes, nil, true, true, false, "not_scheduled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			image := Image{Scope: "reference", Identity: Identity{Reference: "latest"}, ResolvedDigest: "digest", Cache: CacheState{State: tc.cache}, Availability: Availability{State: tc.availability}, Solidification: Solidification{Lifecycle: Lifecycle{Started: tc.started}, Tasks: tc.works}, Evidence: Evidence{ClosureComplete: tc.closure, Freshness: "fresh"}, TagPublished: tc.tag}
			if got := Classify(image, tc.anchored); got != tc.want {
				t.Fatalf("category=%s want=%s", got, tc.want)
			}
			image.Evidence.Freshness = "unknown"
			if Classify(image, true) != "unknown" {
				t.Fatal("stale evidence classified as success")
			}
		})
	}
}
