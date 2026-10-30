package app

import (
	"context"
	"encoding/json"
	"registry-hub/internal/diagnostics"
	"registry-hub/internal/inventory"
	"strings"
	"testing"
	"time"
)

func TestClientUploadCompletionKeepsIdentityAndSanitizesSession(t *testing.T) {
	f := newStagedAcceptanceFixture(t, 1<<20)
	ctx := context.Background()
	state := uploadSession{ID: strings.Repeat("a", 32), Repository: "team/image", Token: "private-token", InternalPath: "/private/upload", CreatedAt: time.Now().UTC()}
	before := uploadTransferView(state)
	u := uploadSessions{store: f.service.store}
	u.recordUploadCompletion(ctx, state, "complete", "sha256:"+strings.Repeat("b", 64), "0-9")
	after, _, err := inventory.Read[transferView](ctx, f.service.store, diagnostics.Prefix+"upload-receipts/"+state.ID+".json")
	if err != nil || after.ID != before.ID || after.TransferID != before.TransferID || after.TransferState != "complete" || after.TotalBytes == nil || *after.TotalBytes != 10 {
		t.Fatalf("upload receipt=%+v err=%v", after, err)
	}
	encoded, _ := json.Marshal(after)
	if strings.Contains(string(encoded), state.Token) || strings.Contains(string(encoded), state.InternalPath) {
		t.Fatal("upload credentials leaked")
	}
}
