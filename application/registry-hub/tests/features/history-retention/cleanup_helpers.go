package historyretention

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/support"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
	"net/http"
	"sort"
	"strings"
	"time"
)

func readCleanupCredentials(ctx context.Context, environment testkit.CaseContext) (cleanupCredentials, error) {
	_, data, err := support.GetSecret(ctx, environment.Namespace, environment.Release+"-external-history-admin")
	if err != nil {
		return cleanupCredentials{}, err
	}
	var configuration support.AdminAuth
	if err := json.Unmarshal(data["config.json"], &configuration); err != nil {
		return cleanupCredentials{}, err
	}
	credentials := cleanupCredentials{}
	found := map[string]bool{}
	for _, item := range configuration.Basic {
		if item.Password == "" || item.Token != "" {
			return cleanupCredentials{}, fmt.Errorf("history test credential is malformed")
		}
		permissions := append([]string(nil), item.Permissions...)
		sort.Strings(permissions)
		credential := testkit.BasicCredential{Username: item.Username, Password: item.Password}
		switch item.Username {
		case "history-operator":
			if strings.Join(permissions, " ") != "history-cleanup observe" {
				return cleanupCredentials{}, fmt.Errorf("history operator permissions are invalid")
			}
			credentials.Operator = credential
		case "history-observer":
			if strings.Join(permissions, " ") != "observe" {
				return cleanupCredentials{}, fmt.Errorf("history observer permissions are invalid")
			}
			credentials.Observer = credential
		case "task-admin":
			if strings.Join(permissions, " ") != "task-admin" {
				return cleanupCredentials{}, fmt.Errorf("task admin permissions are invalid")
			}
			credentials.TaskAdmin = credential
		default:
			return cleanupCredentials{}, fmt.Errorf("unexpected history test principal %q", item.Username)
		}
		if found[item.Username] {
			return cleanupCredentials{}, fmt.Errorf("duplicate history test principal %q", item.Username)
		}
		found[item.Username] = true
	}
	if len(found) != 3 {
		return cleanupCredentials{}, fmt.Errorf("history test Secret does not contain all expected principals")
	}
	return credentials, nil
}

func createPreview(ctx context.Context, environment testkit.CaseContext, credential testkit.BasicCredential, body string) (cleanupPreview, error) {
	status, response, err := call(ctx, environment, &credential, http.MethodPost, "/api/v1/history/cleanup/preview", body)
	if err != nil || status != http.StatusOK {
		return cleanupPreview{}, fmt.Errorf("create cleanup preview status=%d error=%v", status, err)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return cleanupPreview{}, err
	}
	var preview cleanupPreview
	if err := json.Unmarshal(encoded, &preview); err != nil {
		return cleanupPreview{}, err
	}
	return preview, nil
}

func exerciseRangeCleanup(ctx context.Context, environment testkit.CaseContext, operator, observer testkit.BasicCredential) (string, map[string]any, string, error) {
	from := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	if err := seedRangeEvents(ctx, environment, from); err != nil {
		return "", nil, "", err
	}
	rangeRequest, err := json.Marshal(map[string]string{
		"category": "diagnostic_events",
		"from":     from.Format(time.RFC3339Nano),
		"to":       from.Add(time.Hour).Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", nil, "", err
	}
	status, response, err := call(ctx, environment, &operator, http.MethodPost, "/api/v1/history/cleanup/previews", string(rangeRequest))
	if err != nil || status != http.StatusAccepted {
		return "", nil, "", fmt.Errorf("create asynchronous range preview status=%d error=%v", status, err)
	}
	previewID := testkit.String(response["id"])
	if previewID == "" {
		return "", nil, "", fmt.Errorf("asynchronous range preview omitted its ID: %v", response)
	}
	path := "/api/v1/history/cleanup/previews/" + previewID
	if status, _, err := call(ctx, environment, &observer, http.MethodGet, path, ""); err != nil || status != http.StatusNotFound {
		return "", nil, "", fmt.Errorf("foreign principal range preview status=%d error=%v", status, err)
	}
	preview, err := waitForRangePreview(ctx, environment, operator, path)
	if err != nil {
		return "", nil, "", err
	}
	status, retention, err := call(ctx, environment, &observer, http.MethodGet, "/api/v1/history/retention", "")
	known := testkit.Map(testkit.Map(retention["knownEligibleVolume"])["diagnostic_events"])
	if err != nil || status != http.StatusOK || testkit.Int(known["eligible"]) != rangeFixtureEvents || testkit.Int(known["eligibleBytes"]) == 0 {
		return "", nil, "", fmt.Errorf("retention did not report the completed range preview: status=%d counts=%v error=%v", status, known, err)
	}
	limits := testkit.Map(preview["limits"])
	if testkit.Int(limits["maxRangeSeconds"]) != int64((7*24*time.Hour)/time.Second) ||
		testkit.Int(limits["maxSegmentObjects"]) != 10000 || testkit.Int(limits["maxSegmentBytes"]) != 64<<20 ||
		testkit.Int(limits["checkpointObjects"]) != 32 || testkit.Int(limits["storageRequests"]) != 96 ||
		testkit.Int(limits["maxStepSeconds"]) != 5 {
		return "", nil, "", fmt.Errorf("range preview returned unexpected execution limits: %v", limits)
	}
	operationID := ""
	if segmentID := testkit.String(preview["segmentID"]); segmentID != "" && testkit.Int(preview["segmentItems"]) == rangeFixtureEvents {
		submission, marshalErr := json.Marshal(map[string]string{"previewID": previewID, "segmentID": segmentID})
		if marshalErr != nil {
			return "", nil, "", marshalErr
		}
		status, accepted, submitErr := callWithHeaders(ctx, environment, &operator, http.MethodPost,
			"/api/v1/history/cleanup", string(submission), map[string]string{"Idempotency-Key": "range-cleanup-" + previewID})
		if submitErr != nil || status != http.StatusAccepted {
			return "", nil, "", fmt.Errorf("submit asynchronous range segment status=%d error=%v", status, submitErr)
		}
		operationID = testkit.String(accepted["id"])
		if operationID == "" {
			return "", nil, "", fmt.Errorf("range cleanup submission omitted its ID: %v", accepted)
		}
		completed, err := waitForCompletion(ctx, environment, operator, "/api/v1/history/cleanup/"+operationID)
		if err != nil {
			return "", nil, "", err
		}
		if testkit.Int(completed["deleted"]) != rangeFixtureEvents || testkit.Int(completed["missing"]) != 0 {
			return "", nil, "", fmt.Errorf("range cleanup did not delete every seeded event: %v", completed)
		}
	} else {
		return "", nil, "", fmt.Errorf("range preview did not include the seeded events: %v", preview)
	}
	return previewID, preview, operationID, nil
}
