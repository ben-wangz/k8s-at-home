package tasks

import (
	"context"
	"registry-hub/internal/storage/filesystem"
	"strings"
	"testing"
)

func TestBlobReconciliationPreservesStoppedOrStaleRound(t *testing.T) {
	for _, tc := range []struct {
		name       string
		state      BlobIntentState
		cancel     bool
		generation uint64
	}{
		{"cancelled", IntentCancelled, false, 1},
		{"failed", IntentFailed, false, 1},
		{"anchored", IntentAnchored, false, 1},
		{"cancelling", IntentReady, true, 1},
		{"old_generation", IntentReady, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := filesystem.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			records := NewRecords(store)
			ctx := context.Background()
			identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64)}
			intent, created, err := records.EnsureBlobIntentForReconciliation(ctx, identity, 1)
			if err != nil || !created || intent.Round != 1 {
				t.Fatalf("recover missing legacy intent: created=%t round=%d err=%v", created, intent.Round, err)
			}
			intent.State, intent.CancelRequested = tc.state, tc.cancel
			if err := records.SaveBlobIntent(ctx, &intent); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				current, created, err := records.EnsureBlobIntentForReconciliation(ctx, identity, tc.generation)
				if err != nil || created || current.ID != intent.ID || current.Round != 1 || current.State != tc.state || current.CancelRequested != tc.cancel {
					t.Fatalf("reconciliation changed stopped/stale round: id=%s round=%d state=%s cancel=%t created=%t err=%v", current.ID, current.Round, current.State, current.CancelRequested, created, err)
				}
			}
			demands, _, err := records.ListBlobDemands(ctx, intent.ID, "", 32)
			if err != nil || len(demands) != 0 {
				t.Fatalf("reconciliation manufactured demand: %v %v", demands, err)
			}
		})
	}
}

func TestVerifiedBlobIntentKeepsRoundAcrossMediaTypes(t *testing.T) {
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	ctx := context.Background()
	identity := BlobIdentity{Authority: "registry.example", Repository: "team/image", Digest: "sha256:" + strings.Repeat("b", 64)}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "same-verified-bytes", 2)
	if err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentState(ctx, intent.ID, IntentVerified, "", "")
	if err != nil {
		t.Fatal(err)
	}
	current, err := records.SetBlobIntentReady(ctx, intent.ID, "application/vnd.oci.image.config.v1+json", "same-verified-bytes", 2)
	if err != nil || current.ID != intent.ID || current.Round != intent.Round || current.State != IntentVerified {
		t.Fatalf("media type replaced verified bytes: id=%s round=%d state=%s err=%v", current.ID, current.Round, current.State, err)
	}
}
