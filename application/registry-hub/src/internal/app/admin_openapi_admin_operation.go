package app

import (
	"fmt"
	"github.com/danielgtaylor/huma/v2"
	"net/http"
	"reflect"
	"registry-hub/internal/auth"
	"registry-hub/internal/registryapi"
	"strconv"
	"strings"
)

func adminOperation(api huma.API, method, path, id, summary, permission string, responses map[string]*huma.Response, params []*huma.Param, body ...*huma.RequestBody) huma.Operation {
	errorSchema := adminSchema[registryapi.ErrorEnvelope](api)
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		key := strconv.Itoa(status)
		if _, exists := responses[key]; !exists {
			responses[key] = adminJSONResponse(http.StatusText(status), errorSchema)
		}
	}
	operation := huma.Operation{
		Method:      method,
		Path:        path,
		OperationID: id,
		Summary:     summary,
		Description: adminPermissionDescription(permission),
		Tags:        []string{adminTag(path)},
		Security: []map[string][]string{
			{"basicAuth": {}},
			{"bearerAuth": {}},
		},
		Parameters: params,
		Responses:  responses,
	}
	if len(body) > 0 {
		operation.RequestBody = body[0]
	}
	return operation
}

func adminResponses(api huma.API, success map[string]*huma.Response, errorStatuses ...int) map[string]*huma.Response {
	responses := make(map[string]*huma.Response, len(success)+len(errorStatuses))
	for code, response := range success {
		responses[code] = response
	}
	errorSchema := adminSchema[registryapi.ErrorEnvelope](api)
	for _, status := range errorStatuses {
		responses[strconv.Itoa(status)] = adminJSONResponse(http.StatusText(status), errorSchema)
	}
	return responses
}

func adminTag(path string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/tasks"):
		return "Tasks"
	case strings.HasPrefix(path, "/api/v1/gc"):
		return "Garbage collection"
	default:
		return "Referrers"
	}
}

func adminSchema[T any](api huma.API) *huma.Schema {
	typeOf := reflect.TypeFor[T]()
	return api.OpenAPI().Components.Schemas.Schema(typeOf, true, typeOf.Name())
}

func adminJSONResponse(description string, schema *huma.Schema) *huma.Response {
	return &huma.Response{
		Description: description,
		Content: map[string]*huma.MediaType{
			"application/json": {Schema: schema},
		},
	}
}

func adminJSONRequest(description string, schema *huma.Schema) *huma.RequestBody {
	return &huma.RequestBody{
		Description: description,
		Required:    true,
		Content: map[string]*huma.MediaType{
			"application/json": {Schema: schema},
		},
	}
}

func adminPathParam(name, description string) *huma.Param {
	return &huma.Param{Name: name, In: "path", Required: true, Description: description, Schema: &huma.Schema{Type: "string"}}
}

func adminStringQueryParam(name, description string, required bool, enum ...any) *huma.Param {
	schema := &huma.Schema{Type: "string", Enum: enum}
	return &huma.Param{Name: name, In: "query", Required: required, Description: description, Schema: schema}
}

func adminIntegerQueryParam(name, description string, min, max int, defaultValue int) *huma.Param {
	minimum, maximum := float64(min), float64(max)
	schema := &huma.Schema{Type: "integer", Minimum: &minimum, Maximum: &maximum, Default: defaultValue}
	return &huma.Param{Name: name, In: "query", Description: description, Schema: schema}
}

func adminTaskQueryParams() []*huma.Param {
	since := adminStringQueryParam("since", "Include tasks created at or after this RFC 3339 timestamp.", false)
	since.Schema.Format = "date-time"
	before := adminStringQueryParam("before", "Include tasks created before this RFC 3339 timestamp.", false)
	before.Schema.Format = "date-time"
	return []*huma.Param{
		adminStringQueryParam("state", "Filter by task state.", false, "pending", "running", "retry_wait", "waiting_cache", "waiting_dependencies", "waiting_capacity", "verified_local", "solidified", "failed", "cancelled", "repair_pending"),
		adminStringQueryParam("authority", "Filter by upstream registry authority.", false),
		adminStringQueryParam("repository", "Filter by repository.", false),
		adminStringQueryParam("digest", "Filter by root digest or reference.", false),
		since,
		before,
		adminIntegerQueryParam("limit", "Maximum number of tasks to return (default 100).", 1, 1000, 100),
		adminStringQueryParam("token", "Opaque pagination token returned by a previous page.", false),
	}
}

func adminTaskDetailQueryParams() []*huma.Param {
	return []*huma.Param{
		adminIntegerQueryParam("graphLimit", "Maximum graph entries per list (default 100).", 1, 100, 100),
		adminStringQueryParam("graphToken", "Continuation token for the task graph map.", false),
		adminStringQueryParam("graphNodesToken", "Opaque continuation token for persisted layered graph nodes.", false),
		adminStringQueryParam("checkpointsToken", "Continuation token for the task checkpoint map.", false),
		adminStringQueryParam("ownerSessionID", "Protection session whose owner IDs should be returned; defaults to the first task session.", false),
		adminIntegerQueryParam("ownerLimit", "Maximum number of protection owners to return (default 100).", 1, 100, 100),
		adminStringQueryParam("ownerToken", "Opaque continuation token for protection owner IDs.", false),
	}
}

func adminReferrerQueryParams() []*huma.Param {
	limit := adminIntegerQueryParam("limit", "Maximum number of referrers and repair intents per page (default 100).", 1, 500, 100)
	return []*huma.Param{
		adminStringQueryParam("repository", "Repository name, optionally prefixed by its registry authority.", true),
		adminStringQueryParam("subject", "Subject manifest digest.", true),
		limit,
		adminStringQueryParam("lastReferrer", "Opaque cursor for the next referrer page.", false),
		adminStringQueryParam("lastIntent", "Opaque cursor for the next repair intent page.", false),
	}
}

func isAdminDocumentationPath(path string) bool {
	switch path {
	case "/docs", "/openapi.json", "/openapi.yaml", "/openapi-3.0.json", "/openapi-3.0.yaml":
		return true
	default:
		return false
	}
}

func (s *Service) authorizeAdminDocumentation(writer http.ResponseWriter, request *http.Request) bool {
	principal, ok := auth.Authenticate(request, s.cfg.AdminAuth, false)
	if !ok {
		auth.Challenge(writer, "registry-hub-admin")
		writeAdminError(writer, http.StatusUnauthorized, "UNAUTHORIZED", "management authentication is required")
		return false
	}
	if !principal.Allows(auth.TaskAdmin) && !principal.Allows(auth.GCAdmin) && !principal.Allows(auth.ReferrerPrune) {
		writeAdminError(writer, http.StatusForbidden, "DENIED", "management permission is required")
		return false
	}
	return true
}

func matchesAdminOperation(operations []huma.Operation, method, path string) bool {
	for index := range operations {
		if operations[index].Method == method && matchesOpenAPIPath(operations[index].Path, path) {
			return true
		}
	}
	return false
}

func matchesOpenAPIPath(pattern, path string) bool {
	patternParts := strings.Split(strings.Trim(pattern, "/"), "/")
	pathParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(patternParts) != len(pathParts) {
		return false
	}
	for index, part := range patternParts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			if pathParts[index] == "" {
				return false
			}
			continue
		}
		if pathParts[index] != part {
			return false
		}
	}
	return true
}

func adminPermissionDescription(permission string) string {
	return fmt.Sprintf("Required permission: `%s`.", permission)
}
