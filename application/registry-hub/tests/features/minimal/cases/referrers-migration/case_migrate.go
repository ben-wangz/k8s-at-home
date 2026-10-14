package referrersmigration

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"net/url"
	"strings"
)

func migrate(ctx context.Context, environment testkit.CaseContext, repository, subject string) (migrationResponse, error) {
	requestBody, err := json.Marshal(map[string]any{
		"repository": repository, "subject": subject, "action": "migrate", "complete": true,
	})
	if err != nil {
		return migrationResponse{}, err
	}
	response, err := support.AdminRequest(ctx, environment, http.MethodPost, "/api/v1/referrers/reconcile", bytes.NewReader(requestBody), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return migrationResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		body, _ := testkit.ReadBody(response, 1<<20)
		return migrationResponse{}, fmt.Errorf("referrer migration returned %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	var result migrationResponse
	if _, err := support.DecodeJSON(response, 2<<20, &result); err != nil {
		return migrationResponse{}, err
	}
	return result, nil
}

type migrationState struct {
	State      string
	Generation uint64
}

func readMigrationState(ctx context.Context, environment testkit.CaseContext, repository, subject string) (migrationState, error) {
	query := url.Values{"repository": {repository}, "subject": {subject}}
	response, err := support.AdminRequest(ctx, environment, http.MethodGet, "/api/v1/referrers/reconcile?"+query.Encode(), nil, nil)
	if err != nil {
		return migrationState{}, err
	}
	var result struct {
		LastMigration map[string]any `json:"lastMigration"`
	}
	if _, err := support.DecodeJSON(response, 2<<20, &result); err != nil {
		return migrationState{}, err
	}
	generation, ok := result.LastMigration["generation"].(float64)
	if !ok || generation < 1 {
		return migrationState{}, fmt.Errorf("migration state has no generation")
	}
	return migrationState{State: testkit.String(result.LastMigration["state"]), Generation: uint64(generation)}, nil
}

func fallbackTag(digest string) string {
	algorithm, encoded, _ := strings.Cut(digest, ":")
	return algorithm + "-" + encoded
}

func validDigest(digest string) bool {
	algorithm, encoded, found := strings.Cut(digest, ":")
	if !found || algorithm != "sha256" || len(encoded) != 64 {
		return false
	}
	_, err := hex.DecodeString(encoded)
	return err == nil
}
