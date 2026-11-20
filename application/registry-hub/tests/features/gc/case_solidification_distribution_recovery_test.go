package gc

import "testing"

func TestVerifiedLocalBlobTaskIsNotAnchored(t *testing.T) {
	if blobTaskIsSolidified(map[string]any{"status": "verified_local"}) {
		t.Fatal("verified_local blob task was treated as solidified")
	}
	if !blobTaskIsSolidified(map[string]any{"status": "solidified"}) {
		t.Fatal("solidified blob task was not treated as complete")
	}
}
