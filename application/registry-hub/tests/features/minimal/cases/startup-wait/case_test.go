package startupwait

import (
	"testing"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func TestReplicaSetControlledFiltersFixturePods(t *testing.T) {
	for _, test := range []struct {
		name      string
		owners    []any
		wantMatch bool
	}{
		{
			name: "deployment pod",
			owners: []any{map[string]any{
				"kind": "ReplicaSet", "controller": true,
			}},
			wantMatch: true,
		},
		{
			name:      "fixture pod with copied deployment labels",
			owners:    []any{},
			wantMatch: false,
		},
		{
			name: "non-controller owner reference",
			owners: []any{map[string]any{
				"kind": "ReplicaSet", "controller": false,
			}},
			wantMatch: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pod := testkit.Object{Metadata: map[string]any{"ownerReferences": test.owners}}
			if got := replicaSetControlled(pod); got != test.wantMatch {
				t.Fatalf("replicaSetControlled() = %t, want %t", got, test.wantMatch)
			}
		})
	}
}
