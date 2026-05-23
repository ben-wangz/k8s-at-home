# Agent Task Manager Design

## Goal

Rebuild the current `pith` capabilities inside `/root/code/github/k8s-at-home/application/agent-task-manager/` with these constraints:

1. Functional scope should comprehensively cover the current `/root/code/github/pith/` product.
2. Use a frontend/backend separated architecture.
3. Backend must be written in Go.
4. Project layout should follow the same operational pattern used by `/root/code/github/k8s-at-home/application/sshpiper/`, but split into separate backend and frontend application roots with their own `container/` and `src/` directories, plus a standalone Go CLI component.

This document is the implementation design for that rebuild.

## Reference Baseline

The current `pith` product includes these major surfaces:

- REST API for projects, tasks, comments, sessions, analytics, users, auth, and webhooks.
- Web UI for board/list/detail/activity/session views.
- CLI for task, project, config, search, and session workflows.
- Bun-backed SQL persistence with switchable SQLite or PostgreSQL storage.

The rebuild should preserve the product intent, but not the TypeScript monorepo structure.

## Product Scope To Preserve

### Core domain capabilities

- Users and API keys
- Projects
- Tasks
- Task hierarchy: parent/subtask
- Comments
- Task activity timeline / audit log
- Sessions for human or agent work tracking
- Labels, priority, status, assignee
- Search

### API-facing capabilities

- Bearer auth with API key and session token support
- REST API for all main entities
- Health endpoint
- OpenAPI generation

### Integration capabilities

- Webhooks

### Web capabilities

- Home overview
- Projects index
- Tasks workspace with board/list modes
- Task detail
- Sessions page
- Activity page

### CLI capabilities

- config show/get/set/init
- project list
- task list/create/show/update/comment
- search
- session start/end/list/show

The CLI is agent-first. It is primarily for AI agents and automation, not just for interactive human use.

### Deferred but designed for

- multi-tenant administration
- richer analytics

These can be staged later, but the architecture should leave clean extension points.

## Non-Goals For First Cut

- Full feature parity on day one for every current `pith` endpoint.
- Rebuilding the current TypeScript implementation style.
- Coupling the web UI bundle into the backend binary.

## Target Architecture

Use a separated architecture with three deployable/runtime concerns:

1. Go API server
2. Web frontend SPA
3. SQL database, defaulting to SQLite and optionally PostgreSQL

The backend remains the system of record. The web UI and CLI both talk to the backend API.

## High-Level Design

### Backend

The backend is a Go service under `backend/src/`.

Responsibilities:

- HTTP API
- auth and authorization
- domain logic
- persistence
- search queries
- webhook dispatch
- session tracking
- activity log generation
- OpenAPI exposure

Recommended internal layering:

- `backend/src/cmd/agent-task-manager-api/`: API entrypoint
- `backend/src/internal/config/`: environment and runtime config
- `backend/src/internal/http/`: router, middleware, handlers
- `backend/src/internal/auth/`: API key and token auth
- `backend/src/internal/domain/`: core entities and business rules
- `backend/src/internal/store/`: repository interfaces
- `backend/src/internal/store/bunstore/`: Bun-backed SQL implementation
- `backend/src/internal/search/`: task search query builder
- `backend/src/internal/events/`: domain event model
- `backend/src/internal/integrations/webhook/`: webhook delivery
- `backend/src/internal/service/`: application services for tasks, projects, sessions, users
- `backend/src/pkg/apierrors/`: reusable public error model if needed

This keeps transport, business logic, and storage clearly separated.

### Frontend

The frontend is a separate SPA rooted at `frontend/`, with application code under `frontend/src/`.

Recommended stack:

- React
- TypeScript
- Vite
- a lightweight data-fetching layer using plain fetch or a small query client

Responsibilities:

- authentication UI
- home overview
- projects index
- tasks workspace with board/list flows
- task detail and contextual preview flows
- session snapshot views
- activity views with filtering
- copy-ID and similar workflow helpers

The frontend must only depend on the API contract, not backend implementation details.

The frontend should follow the UI redesign in `ui.md`, not the earlier mock-navigation approach.

### CLI

The CLI should also be rewritten in Go, but live in its own component under `cli/`.

Recommended entrypoint:

- `cli/src/cmd/atmctl/`

Rationale:

- shared auth/config models with the backend
- simpler packaging for automation and agent environments
- one language for backend and CLI
- a standalone CLI boundary allows agent-focused UX and release flow without coupling it to backend deployment

The CLI remains a remote API client, not a direct database client.

The CLI should be designed as an agent CLI first:

- deterministic, script-friendly outputs
- machine-readable JSON modes
- stable command contracts for automation
- explicit exit codes and low-ambiguity error messages

Reference direction: `/root/code/github/bot-cli/`

### Database

Use Bun as the database access layer.

Default runtime storage should be SQLite for local persistence.

The same backend should also support PostgreSQL through configuration.

The compatibility boundary should live in the repository/store layer so domain and HTTP code do not depend on a specific SQL engine.

Recommended initial model groups:

- `users`
- `api_keys`
- `projects`
- `tasks`
- `task_comments`
- `task_labels`
- `task_activities`
- `sessions`
- `session_tasks`
- `webhooks`

Recommended conventions:

- UUID primary keys
- `created_at`, `updated_at`
- soft delete only where truly needed
- status and priority as constrained text values managed at the application layer
- avoid database-specific features in the first cut so SQLite and PostgreSQL stay interchangeable

## Deployment Model

Use a frontend/backend separated deployment in Kubernetes.

### API deployment

- Go API container
- ClusterIP service
- optional ingress exposure
- DB connection via secret-backed env vars

### Web deployment

- static SPA served by nginx or caddy
- separate ClusterIP service
- runtime config should point to API base URL
- ingress can expose both API and web, either split or under one host with path routing

## Directory Layout

Target directory layout:

```text
application/agent-task-manager/
├── chart/
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
├── backend/
│   ├── container/
│   │   ├── api.Containerfile
│   │   └── VERSION
│   └── src/
│       ├── go.mod
│       ├── go.sum
│       ├── cmd/
│       │   └── agent-task-manager-api/
│       └── internal/
├── cli/
│   └── src/
│       ├── go.mod
│       ├── go.sum
│       └── cmd/
│           └── atmctl/
├── frontend/
│   ├── container/
│   │   ├── Containerfile
│   │   ├── nginx.conf
│   │   └── VERSION
│   ├── index.html
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   └── src/
├── README.md
└── design.md
```

### Why this layout

- `chart/` stays at the application root because deployment still treats this as one product.
- `backend/` and `frontend/` each mirror the `sshpiper` pattern inside their own boundary.
- `cli/` stays separate because it is not a deployed in-cluster runtime component.
- backend and frontend maintain separate container images and separate build logic.
- the build context should remain `application/agent-task-manager/`, with per-image container files under each component root.
- the CLI is intentionally not containerized in this project layout.

## Helm Chart Design

The chart should manage a complete installable application stack for this service.

### Expected chart-managed resources

- API Deployment
- API Service
- Web Deployment
- Web Service
- optional Ingress
- Secret references for DB/auth/runtime config
- ConfigMap for frontend runtime config if needed
- optional PVCs only if later features require local durable state

### Chart values shape

Follow `sshpiper/chart/values.yaml` style:

- `replicas`
- `commonLabels`
- `commonAnnotations`
- `podLabels`
- `podAnnotations`
- `imagePullSecrets`
- `affinity`
- `nodeSelector`
- `tolerations`
- `podSecurityContext`
- `containerSecurityContext`
- `resources`
- `resourcesPreset`
- `extraEnvVars`
- `extraEnvVarsSecret`
- `extraVolumes`
- `extraVolumeMounts`
- `extraDeploy`

Application-specific chart sections:

- `backend.api.image.*`
- `frontend.image.*`
- `service.api.*`
- `service.web.*`
- `ingress.*`
- `database.*`
- `auth.*`
- `featureFlags.*`
- `web.runtimeConfig.*`

### Chart deployment choice

Use one chart with multiple deployments instead of separate charts.

Reason:

- this is one product
- users expect one install path
- backend/web versioning should stay aligned

The CLI is not part of the chart.

## Container Strategy

Follow the `sshpiper` pattern of explicit container files under `container/`.

### Backend API container

- multi-stage Go build
- Dockerfile path: `backend/container/api.Containerfile`
- small runtime image, preferably distroless or alpine only if debugging needs justify it
- produces one API binary

### Frontend container

- Node build stage
- Dockerfile path: `frontend/container/Containerfile`
- nginx runtime stage
- nginx config path: `frontend/container/nginx.conf`
- serves static files
- proxies nothing by default if API base URL is configured explicitly

### Versioning

Maintain separate version files for backend and frontend image/version coordination:

- `backend/container/VERSION`
- `frontend/container/VERSION`

## Backend API Design

### Initial endpoint groups

- `/healthz`
- `/readyz`
- `/api/v1/auth/*`
- `/api/v1/users/*`
- `/api/v1/projects/*`
- `/api/v1/tasks/*`
- `/api/v1/sessions/*`
- `/api/v1/search`
- `/api/v1/webhooks/*`

### Core task operations

- create task
- update task
- list tasks with filters
- show task detail with parent, subtasks, comments, activity
- add comment
- create subtasks
- reparent task

The rebuild should explicitly support task reparenting, which is a known gap in the current `pith` workflow.

### Auth model

Support:

- long-lived API keys for agents and automation
- browser login session or JWT-based token flow for humans

Recommended roles initially:

- `admin`
- `member`
- `agent`

## Search Design

Use Bun as the common query layer and keep the first search implementation database-neutral.

Search targets:

- task title
- task description
- comments
- labels

Optional later evolution:

- database-specific search indexes only if later usage justifies them
- ranking improvements
- semantic search sidecar, if real use cases justify it

## Session and Activity Model

Sessions are a first-class model.

For the current target workflow, a session is treated as a snapshot of an AI agent session, with OpenCode as the reference implementation.

### Session snapshot model

Each stored session snapshot should capture:

- `snapshot_id`: platform-generated UUID for this snapshot record
- `type`: session source type, currently `opencode`
- `title`: human-readable snapshot title
- `description`: short operator-facing description
- `artifact_name`: uploaded session export artifact name
- `artifact_path`: stored export artifact location, supporting `s3://...` and filesystem paths

The design intentionally does not require us to parse or persist the exported OpenCode session JSON in the database in the first implementation. The artifact location is the source of truth for the full exported payload.

### Notes

- `snapshot_id` is the primary ID in agent-task-manager for session snapshot records.
- `type=opencode` is required so future agent session types can coexist without changing the storage model.
- The exported session file itself should be accessed through `artifact_path`, not duplicated into database fields.
- Any deeper transcript inspection can be done by reading the artifact when needed, rather than by storing the full export body in relational storage.

### Session UI expectations

The Sessions UI should provide:

- upload entry for a session export artifact
- download action for a stored session export artifact
- list/detail presentation using only the minimal visible fields:
  - `title`
  - `snapshot_id`
  - `description`
  - `artifact_name`
  - `artifact_path`

The UI should not require first-version parsing of the export body into many visible structured fields.

The current frontend direction treats Sessions as a standalone snapshot registry page, not as a project/task execution board.

Activity log entries should be generated for:

- task created
- task updated
- status changed
- assignee changed
- comment added
- session started
- session ended
- subtask created
- parent changed

This preserves the auditability that agents need.

## Frontend Design

### Primary views

- Home
- Projects
- Tasks workspace
- Task detail page
- Sessions
- Activity

### Application shell

The web UI should use a stable application shell instead of top-banner demo navigation.

Primary shell structure:

- left sidebar navigation
- persistent top utility bar
- main workspace region
- optional contextual right-side detail panel

Navigation order:

1. Home
2. Projects
3. Tasks
4. Sessions
5. Activity

`Board` should live inside the `Tasks` workspace as a view mode, not as a primary top-level section.

### Frontend rules

- all mutations go through backend APIs
- no hidden local-only state that changes domain truth
- explicit empty, loading, and auth-error states
- task IDs should be easy to copy anywhere task references appear
- preserve context when moving between list/board selection, preview, and full task detail
- prefer dense operational layouts over oversized showcase cards
- use one shared filter interaction model across Tasks and Activity

### Initial UI feature expectations

- sidebar navigation with top utility bar
- tasks workspace with internal view switching for `Board`, `List`, and assigned-task focused flows
- optional right-side contextual preview panel for task and activity inspection
- copy task ID button on task cards, rows, and task detail page
- visible auth failure states instead of silent empty lists
- parent/subtask visibility near the top of task detail
- session snapshot upload/download affordances in a registry-style view
- activity filtering by project, task, and label with `NOT` support and sort direction control
- active filters represented explicitly as removable chips
- clear empty states that explain scope and next actions

## Migration Strategy From Pith

Do not attempt a line-by-line port.

### Recommended migration phases

1. **Schema and core API**
   - projects, tasks, comments, sessions, users, auth
2. **CLI parity for main workflows**
   - list/create/show/update/comment/search/session
3. **Web UI parity for main workflows**
   - home, projects, board, detail, sessions, activity
4. **Integrations**
   - webhooks
### Data migration approach

If migration from an existing `pith` database becomes necessary later:

- write explicit import tooling
- map TypeScript-era enums/statuses carefully
- do not block the first implementation on live migration tooling

## Recommended Implementation Phases

### Phase 0: skeleton

- create directory structure
- create Go module
- create chart skeleton
- create container skeletons
- create frontend skeleton

### Phase 1: backend MVP

- config
- DB connection
- migrations
- auth middleware
- projects/tasks/comments/sessions APIs

### Phase 2: CLI MVP

- init/config
- project list
- task list/create/show/update/comment
- session start/end/list/show
- search

### Phase 3: web MVP

- login
- application shell with sidebar and top utility bar
- home overview
- projects index
- tasks list view
- task preview panel
- task detail
- sessions snapshot registry
- activity filtering and sorting
- copy-ID UX
- board view after list/detail structure is stable

### Phase 4: integrations

- webhooks
- analytics basics

## Key Design Decisions

1. Use Go for backend and CLI.
2. Use a separate SPA frontend rooted at `frontend/`.
3. Use the backend API as the system of record, with SQLite by default and PostgreSQL available by configuration.
4. Use one Helm chart with multiple deployments.
5. Follow `sshpiper` conventions for `chart/`, `container/`, and `src/`, adapted to separate backend/frontend roots.
6. Keep the CLI as a standalone agent CLI, not a containerized runtime component.
7. Treat task reparenting as a required first-class feature, not an afterthought.
8. Prefer a staged rebuild over a risky full-port rewrite.

## Open Questions

These do not block the design document, but should be settled before implementation starts:

1. Should the web UI be hosted on a separate subdomain, or behind the same ingress as the API?
2. Should API auth use JWT access tokens only for browsers, or also support refresh tokens from the start?

## Immediate Next Step

After this design is accepted, create the initial filesystem skeleton under `application/agent-task-manager/` and start with Phase 0.
