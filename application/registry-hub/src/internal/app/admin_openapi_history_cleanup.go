package app

import (
	"github.com/danielgtaylor/huma/v2"
	"net/http"
	"registry-hub/internal/auth"
)

func historyCleanupPreviewRequestSchema() *huma.Schema {
	minimum, maximum := 1, len(historyCleanupCategories)
	enum := make([]any, 0, len(historyCleanupCategories))
	for _, category := range historyCleanupCategories {
		enum = append(enum, category)
	}
	return &huma.Schema{
		Type:                 "object",
		Required:             []string{"categories", "cutoff"},
		AdditionalProperties: false,
		Properties: map[string]*huma.Schema{
			"categories": {
				Type:        "array",
				MinItems:    &minimum,
				MaxItems:    &maximum,
				UniqueItems: true,
				Items:       &huma.Schema{Type: "string", Enum: enum},
			},
			"cutoff": {Type: "string", Format: "date-time", Description: "UTC cutoff in RFC 3339 format."},
		},
	}
}

func historyCleanupRangeRequestSchema() *huma.Schema {
	enum := make([]any, 0, len(historyCleanupCategories))
	for _, category := range historyCleanupCategories {
		enum = append(enum, category)
	}
	return &huma.Schema{
		Type: "object", Required: []string{"category", "from", "to"}, AdditionalProperties: false,
		Properties: map[string]*huma.Schema{
			"category": {Type: "string", Enum: enum},
			"from":     {Type: "string", Format: "date-time", Description: "Inclusive UTC start time."},
			"to":       {Type: "string", Format: "date-time", Description: "Exclusive UTC end time; the requested range may span at most seven days."},
			"cursor":   {Type: "string", Description: "Opaque continuation returned when another executable segment remains."},
		},
	}
}

func adminHistoryCleanupOperations(api huma.API) []huma.Operation {
	return []huma.Operation{
		adminOperation(api, http.MethodGet, "/api/v1/history/retention", "getHistoryRetention", "Get effective history retention policy and latest cleanup result", auth.Observe,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Effective retention policy, configured duration values, and latest cleanup evidence.", adminSchema[historyCleanupRetentionView](api))}, http.StatusServiceUnavailable), nil),
		adminOperation(api, http.MethodPost, "/api/v1/history/cleanup/preview", "previewHistoryCleanup", "Preview cleanup eligibility for server-defined history categories", auth.HistoryCleanup,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Principal-bound cleanup preview.", adminSchema[historyCleanupPreview](api))}, http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable), nil,
			adminJSONRequest("Categories and UTC cutoff. Object keys and storage prefixes are not accepted.", historyCleanupPreviewRequestSchema())),
		adminOperation(api, http.MethodPost, "/api/v1/history/cleanup/previews", "startHistoryCleanupRangePreview", "Start an asynchronous preview for a bounded UTC history range", auth.HistoryCleanup,
			adminResponses(api, map[string]*huma.Response{"202": adminJSONResponse("Durable range preview with effective range, object and byte segment limits, checkpoint size, storage request and time budgets, status URL, and any terminal-history index backfill still pending.", adminSchema[historyCleanupRangePreview](api))}, http.StatusBadRequest, http.StatusConflict, http.StatusTooManyRequests, http.StatusServiceUnavailable), nil,
			adminJSONRequest("One server-defined category and a half-open UTC range. A continuation cursor resumes from the prior completed segment.", historyCleanupRangeRequestSchema())),
		adminOperation(api, http.MethodGet, "/api/v1/history/cleanup/previews/{id}", "getHistoryCleanupRangePreview", "Get asynchronous history cleanup preview progress and its executable segment", auth.Observe,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Range preview progress, protected reasons, pending history-index backfill workers, exact segment ID, and continuation cursor.", adminSchema[historyCleanupRangePreview](api))}, http.StatusBadRequest, http.StatusGone, http.StatusNotFound, http.StatusServiceUnavailable), []*huma.Param{adminPathParam("id", "Range preview identifier.")}),
		adminOperation(api, http.MethodPost, "/api/v1/history/cleanup", "startHistoryCleanup", "Start cleanup from an unexpired preview", auth.HistoryCleanup,
			adminResponses(api, map[string]*huma.Response{"202": adminJSONResponse("Durable cleanup task.", adminSchema[historyCleanupTaskView](api))}, http.StatusBadRequest, http.StatusConflict, http.StatusGone, http.StatusNotFound, http.StatusTooManyRequests, http.StatusServiceUnavailable),
			[]*huma.Param{{Name: "Idempotency-Key", In: "header", Required: true, Description: "Client-generated key for this cleanup request.", Schema: &huma.Schema{Type: "string", MinLength: func() *int { value := 1; return &value }(), MaxLength: func() *int { value := 128; return &value }()}}},
			adminJSONRequest("Accept a preview ID for execution.", adminSchema[historyCleanupExecutionRequest](api))),
		adminOperation(api, http.MethodGet, "/api/v1/history/cleanup/{id}", "getHistoryCleanup", "Get durable cleanup task state and counters", auth.Observe,
			adminResponses(api, map[string]*huma.Response{"200": adminJSONResponse("Cleanup task state and counters.", adminSchema[historyCleanupTaskView](api))}, http.StatusBadRequest, http.StatusGone, http.StatusNotFound, http.StatusServiceUnavailable), []*huma.Param{adminPathParam("id", "Cleanup operation identifier.")}),
	}
}
