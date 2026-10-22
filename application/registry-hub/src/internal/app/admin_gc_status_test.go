package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"registry-hub/internal/auth"
	"registry-hub/internal/config"
	"registry-hub/internal/storage/filesystem"
)

func TestAdminGCStatusPagesProtectionOwnersAndBlockedResponse(t *testing.T) {
	ctx := context.Background()
	store, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	gate, err := newWriteGate(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := mustGateTestID(t)
	firstOwner, secondOwner := mustGateTestID(t), mustGateTestID(t)
	if err := gate.AdmitProtection(ctx, sessionID, "platform:sha256:abc", firstOwner, 1024, 4, 4096); err != nil {
		t.Fatal(err)
	}
	if err := gate.AddProtectionOwner(ctx, sessionID, secondOwner); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		ctx: ctx,
		cfg: config.Config{GCMode: "local", AdminAuth: config.Auth{Basic: []config.Credential{{
			Username: "operator", Password: "secret", Permissions: []string{auth.GCAdmin},
		}}}},
		gate: gate,
	}
	service.gc = &gcController{service: service}

	ownerRequest := httptest.NewRequest(http.MethodGet, "/api/v1/gc?ownersFor="+sessionID+"&ownerLimit=1", nil)
	ownerRequest.SetBasicAuth("operator", "secret")
	ownerResponse := httptest.NewRecorder()
	service.handleAdmin(ownerResponse, ownerRequest)
	if ownerResponse.Code != http.StatusOK {
		t.Fatalf("GC owner query returned %d: %s", ownerResponse.Code, ownerResponse.Body.String())
	}
	var ownerPage struct {
		OwnerSessionID  string   `json:"ownerSessionID"`
		Owners          []string `json:"owners"`
		OwnersNextToken string   `json:"ownersNextToken"`
	}
	if err := json.Unmarshal(ownerResponse.Body.Bytes(), &ownerPage); err != nil {
		t.Fatal(err)
	}
	if ownerPage.OwnerSessionID != sessionID || len(ownerPage.Owners) != 1 || ownerPage.OwnersNextToken == "" {
		t.Fatalf("GC owner page = %#v", ownerPage)
	}

	blockedRequest := httptest.NewRequest(http.MethodPost, "/api/v1/gc", nil)
	blockedRequest.SetBasicAuth("operator", "secret")
	blockedResponse := httptest.NewRecorder()
	service.handleAdmin(blockedResponse, blockedRequest)
	if blockedResponse.Code != http.StatusConflict {
		t.Fatalf("blocked GC returned %d: %s", blockedResponse.Code, blockedResponse.Body.String())
	}
	var blocked struct {
		Errors []struct {
			Code   string `json:"code"`
			Detail struct {
				Blockers []protectionSessionStatus `json:"blockers"`
				Gate     gateRecord                `json:"gate"`
			} `json:"detail"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(blockedResponse.Body.Bytes(), &blocked); err != nil {
		t.Fatal(err)
	}
	if len(blocked.Errors) != 1 || blocked.Errors[0].Code != "GC_BLOCKED" || len(blocked.Errors[0].Detail.Blockers) != 1 || blocked.Errors[0].Detail.Blockers[0].OwnerCount != 2 {
		t.Fatalf("blocked GC details = %#v", blocked)
	}
}
