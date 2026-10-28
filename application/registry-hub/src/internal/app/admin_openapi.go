package app

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"net/http"
	"reflect"
	"registry-hub/internal/artifact"
	"registry-hub/internal/tasks"
	"time"
)

type adminTaskListResponse struct {
	Items     []adminTaskView `json:"items"`
	NextToken string          `json:"nextToken"`
}

type adminTaskView struct {
	tasks.Task
	Graph                        map[string]tasks.GraphEntry `json:"graph,omitempty"`
	Checkpoints                  map[string]string           `json:"checkpoints,omitempty"`
	GraphNodes                   []tasks.GraphNode           `json:"graphNodes,omitempty"`
	GraphNextToken               string                      `json:"graphNextToken,omitempty"`
	GraphNodesNextToken          string                      `json:"graphNodesNextToken,omitempty"`
	CheckpointsNextToken         string                      `json:"checkpointsNextToken,omitempty"`
	GCProtectionActive           bool                        `json:"gcProtectionActive"`
	ProtectionSessions           []protectionSessionStatus   `json:"protectionSessions,omitempty"`
	ReleasedProtectionSessionIDs []string                    `json:"releasedProtectionSessionIDs,omitempty"`
	OwnerSessionID               string                      `json:"ownerSessionID,omitempty"`
	Owners                       []string                    `json:"owners,omitempty"`
	OwnersNextToken              string                      `json:"ownersNextToken,omitempty"`
}

type adminTaskCleanupResponse struct {
	Removed               int `json:"removed"`
	UploadSessionsRemoved int `json:"uploadSessionsRemoved"`
	CacheRemoved          int `json:"cacheRemoved"`
	OrphanCacheRemoved    int `json:"orphanCacheRemoved"`
	StorageOrphanRemoved  int `json:"storageOrphanRemoved"`
}

type adminGCStatusResponse struct {
	Gate            gateRecord                `json:"gate"`
	Run             gcRun                     `json:"run"`
	Sessions        []protectionSessionStatus `json:"sessions"`
	NextToken       string                    `json:"nextToken"`
	OwnerSessionID  string                    `json:"ownerSessionID,omitempty"`
	Owners          []string                  `json:"owners,omitempty"`
	OwnersNextToken string                    `json:"ownersNextToken,omitempty"`
}

type adminGCConflictDetail struct {
	Gate        *gateRecord               `json:"gate,omitempty"`
	Blockers    []protectionSessionStatus `json:"blockers,omitempty"`
	NextToken   string                    `json:"nextToken,omitempty"`
	Reason      string                    `json:"reason,omitempty"`
	Unavailable bool                      `json:"unavailable,omitempty"`
}

type adminGCConfirmationRequest struct {
	ConfirmedNoExecutor bool `json:"confirmedNoExecutor" doc:"Confirm that no other GC executor is active."`
}

type adminReferrerIntentView struct {
	Referrer      string    `json:"referrer"`
	Operation     string    `json:"operation"`
	Retryable     bool      `json:"retryable"`
	Attempts      int       `json:"attempts"`
	NextAttemptAt time.Time `json:"nextAttemptAt"`
	ErrorClass    string    `json:"errorClass"`
	Generation    uint64    `json:"generation"`
}

type adminReferrerDescriptors []json.RawMessage

func (adminReferrerDescriptors) Schema(registry huma.Registry) *huma.Schema {
	typeOf := reflect.TypeFor[artifact.Descriptor]()
	return &huma.Schema{Type: "array", Items: registry.Schema(typeOf, true, typeOf.Name())}
}

type adminReferrerReconcileResponse struct {
	Repository    string                     `json:"repository"`
	Subject       string                     `json:"subject"`
	Source        string                     `json:"source"`
	IndexDigest   string                     `json:"indexDigest"`
	Referrers     adminReferrerDescriptors   `json:"referrers"`
	NextReferrer  string                     `json:"nextReferrer"`
	RepairIntents []adminReferrerIntentView  `json:"repairIntents"`
	NextIntent    string                     `json:"nextIntent"`
	LastMigration adminReferrerMigrationView `json:"lastMigration"`
}

type adminReferrerMigrationRequired struct {
	State string `json:"state"`
}

type adminReferrerMigrationComplete struct {
	State               string    `json:"state"`
	Generation          uint64    `json:"generation"`
	Referrers           int       `json:"referrers"`
	FallbackIndexDigest string    `json:"fallbackIndexDigest"`
	NativeIndexDigest   string    `json:"nativeIndexDigest"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type adminReferrerMigrationView map[string]any

func (adminReferrerMigrationView) Schema(registry huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{
		{
			OneOf: []*huma.Schema{
				registry.Schema(reflect.TypeFor[adminReferrerMigrationRequired](), true, ""),
				registry.Schema(reflect.TypeFor[adminReferrerMigrationComplete](), true, ""),
			},
		},
		{Type: "null"},
	}}
}

type adminReferrerMutationResponse struct {
	Action    string `json:"action"`
	Processed int    `json:"processed"`
	Source    string `json:"source"`
}

type adminReferrerReplaceResponse struct {
	Action    string `json:"action"`
	Complete  bool   `json:"complete"`
	Referrers int    `json:"referrers"`
	Source    string `json:"source"`
}

type adminReferrerMigrationResponse struct {
	Action              string `json:"action"`
	Complete            bool   `json:"complete"`
	Generation          uint64 `json:"generation"`
	Referrers           int    `json:"referrers"`
	NativeIndexDigest   string `json:"nativeIndexDigest"`
	FallbackIndexDigest string `json:"fallbackIndexDigest"`
}

type adminReferrerRetryResponse struct {
	Action    string `json:"action"`
	Scheduled int    `json:"scheduled"`
}

func (s *Service) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	config := huma.DefaultConfig("Registry Hub Management API", "v1")
	config.DocsPath = ""
	config.SchemasPath = ""
	api := humago.New(mux, config)
	api.OpenAPI().Info.Description = "Authenticated management endpoints. Each operation lists its required permission. Basic and Bearer credentials are supported."
	api.OpenAPI().Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"basicAuth":  {Type: "http", Scheme: "basic"},
		"bearerAuth": {Type: "http", Scheme: "bearer"},
	}
	operations := s.registerAdminOpenAPIOperations(api)
	mux.HandleFunc("GET /docs", func(writer http.ResponseWriter, request *http.Request) {
		s.serveAdminDocs(writer, api)
	})

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isAdminDocumentationPath(request.URL.Path) {
			if !s.authorizeAdminDocumentation(writer, request) {
				return
			}
			mux.ServeHTTP(writer, request)
			return
		}
		if matchesAdminOperation(operations, request.Method, request.URL.Path) {
			mux.ServeHTTP(writer, request)
			return
		}
		s.handleAdmin(writer, request)
	})
}

func (s *Service) registerAdminOpenAPIOperations(api huma.API) []huma.Operation {
	operations := append(adminOpenAPIOperations(api), adminInventoryOperations(api)...)
	for index := range operations {
		operation := &operations[index]
		api.OpenAPI().AddOperation(operation)
		api.Adapter().Handle(operation, func(ctx huma.Context) {
			request, writer := humago.Unwrap(ctx)
			s.handleAdmin(writer, request)
		})
	}
	return operations
}

func (s *Service) serveAdminDocs(writer http.ResponseWriter, api huma.API) {
	spec, err := json.Marshal(api.OpenAPI())
	if err != nil {
		http.Error(writer, "OpenAPI document could not be generated", http.StatusInternalServerError)
		return
	}
	script := "window.onload = function () { SwaggerUIBundle({ dom_id: '#swagger-ui', spec: " + string(spec) + " }); };"
	hash := sha256.Sum256([]byte(script))
	cspHash := base64.StdEncoding.EncodeToString(hash[:])
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; base-uri 'none'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; sandbox allow-same-origin allow-scripts allow-popups allow-popups-to-escape-sandbox allow-forms allow-downloads; script-src https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js 'sha256-"+cspHash+"'; style-src https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css")
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="referrer" content="no-referrer">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Registry Hub Management API</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui.css" crossorigin integrity="sha384-KX9Rx9vM1AmUNAn07bPAiZhFD4C8jdNgG6f5MRNvR+EfAxs2PmMFtUUazui7ryZQ">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5.31.1/swagger-ui-bundle.js" crossorigin integrity="sha384-o9idN8HE6/V6SAewgnr6/5nz7+Npt5J0Cb4tNyXK8pycsVmgl1ZNbRS7tlEGxd+J"></script>
    <script>%s</script>
  </body>
</html>`, script)
}
