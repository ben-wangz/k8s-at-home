package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	"registry-hub/internal/storage/filesystem"
)

func TestNewRootDemandJoinsVerifiedSharedBlobIntent(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	identity := BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("a", 64),
	}

	completed, created, err := records.EnsureBlobIntent(ctx, identity, 7, "")
	if err != nil || !created {
		t.Fatalf("initial blob intent = %#v created=%t err=%v", completed, created, err)
	}
	completed, err = records.SetBlobIntentReady(ctx, completed.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil {
		t.Fatal(err)
	}
	completed, err = records.SetBlobIntentState(ctx, completed.ID, IntentVerified, "", "")
	if err != nil {
		t.Fatal(err)
	}

	joined, created, err := records.EnsureBlobIntent(ctx, identity, 7, "")
	if err != nil || created || joined.ID != completed.ID || joined.Round != completed.Round || joined.State != IntentVerified {
		t.Fatalf("late root intent = %#v created=%t err=%v; want completed round %q reused", joined, created, err, completed.ID)
	}
	rootID := strings.Repeat("b", 32)
	demandID := strings.Repeat("c", 32)
	sessionID := strings.Repeat("d", 32)
	if err := records.LinkBlobDemandScope(ctx, joined.ID, demandID, rootID, "root-scope", sessionID, 64); err != nil {
		t.Fatalf("link late root demand: %v", err)
	}

	demands, next, err := records.ListBlobDemandRecords(ctx, joined.ID, "", 10)
	if err != nil || next != "" || len(demands) != 1 {
		t.Fatalf("verified blob demands = %#v next=%q err=%v; want only the late root demand", demands, next, err)
	}
	if demands[0].DemandID != demandID || demands[0].RootTaskID != rootID || demands[0].ScopeID != "root-scope" || demands[0].SessionID != sessionID {
		t.Fatalf("late root demand = %#v; want its scope and protection identity preserved", demands[0])
	}
	current, err := records.GetBlobIntentByIdentity(ctx, identity)
	if err != nil || current.ID != completed.ID || current.Round != completed.Round || current.State != IntentVerified {
		t.Fatalf("current blob intent = %#v err=%v; want the original verified round", current, err)
	}
}

func TestDuplicateBlobDemandDoesNotRenewIdleExpiry(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	identity := BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("e", 64),
	}
	intent, _, err := records.EnsureBlobIntent(ctx, identity, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	demandID := strings.Repeat("f", 32)
	if err := records.LinkBlobDemand(ctx, intent.ID, demandID); err != nil {
		t.Fatal(err)
	}
	before, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := before.ExpiresAt.Sub(before.LastProgressAt); got != 24*time.Hour {
		t.Fatalf("initial idle lifetime = %s, want 24h", got)
	}
	if err := records.LinkBlobDemand(ctx, intent.ID, demandID); err != nil {
		t.Fatal(err)
	}
	after, err := records.GetBlobIntent(ctx, intent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.LastProgressAt.Equal(before.LastProgressAt) || !after.ExpiresAt.Equal(before.ExpiresAt) {
		t.Fatalf("duplicate demand changed progress/expiry from %s/%s to %s/%s", before.LastProgressAt, before.ExpiresAt, after.LastProgressAt, after.ExpiresAt)
	}
}

func TestBlobIntentStateChangeResetsNotificationAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	records := NewRecords(store)
	intent, _, err := records.EnsureBlobIntent(ctx, BlobIdentity{
		Authority: "ghcr.io", Repository: "team/image", Digest: "sha256:" + strings.Repeat("f", 64),
	}, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	intent.WakeAcknowledged = true
	if err := records.SaveBlobIntent(ctx, &intent); err != nil {
		t.Fatal(err)
	}
	intent, err = records.SetBlobIntentReady(ctx, intent.ID, "application/octet-stream", "cache-v1", 64)
	if err != nil || intent.WakeAcknowledged {
		t.Fatalf("cache-ready transition = %#v err=%v, want a pending notification", intent, err)
	}
	for _, state := range []BlobIntentState{IntentRunning, IntentVerified, IntentAnchored} {
		intent.WakeAcknowledged = true
		if err := records.SaveBlobIntent(ctx, &intent); err != nil {
			t.Fatal(err)
		}
		unchanged, err := records.SetBlobIntentState(ctx, intent.ID, intent.State, "", "")
		if err != nil || !unchanged.WakeAcknowledged {
			t.Fatalf("duplicate state changed acknowledgement: %#v err=%v", unchanged, err)
		}
		intent, err = records.SetBlobIntentState(ctx, intent.ID, state, "", "")
		if err != nil || intent.WakeAcknowledged {
			t.Fatalf("transition to %s = %#v err=%v, want a pending notification", state, intent, err)
		}
	}
}
