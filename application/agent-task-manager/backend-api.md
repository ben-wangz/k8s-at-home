# Agent Task Manager Backend API

## Goal

This document defines the backend API surface for `agent-task-manager`.

The focus here is not low-level schema detail. The goal is to make each endpoint group's purpose and behavior clear enough to guide backend implementation, frontend integration, and CLI design.

## Principles

1. The API is the system of record.
2. The web frontend and agent CLI both use the same backend API.
3. Endpoints should be explicit and predictable for automation.
4. Session export artifacts are stored by path, not parsed into database-first transcript models.
5. Activity is a first-class timeline, not just a derived view inside task detail.

## API Base

Recommended base path:

```text
/api/v1
```

## Authentication

All protected endpoints use Bearer authentication.

Supported auth modes:

- API key for agents and automation
- browser-oriented auth token or session token for human UI

## Endpoint Groups

### 1. Health And Service Status

These endpoints are for infrastructure, readiness checks, and operational monitoring.

#### `GET /healthz`

Purpose:

- confirm the API process is alive
- used by liveness probes and basic uptime checks

#### `GET /readyz`

Purpose:

- confirm the API is ready to serve traffic
- should reflect dependencies needed for normal operation, such as database readiness

## 2. Authentication And Identity

These endpoints establish who is calling the system and what they are allowed to do.

#### `POST /auth/login`

Purpose:

- authenticate a human user for the web UI
- return a session or token usable by the frontend

#### `POST /auth/logout`

Purpose:

- invalidate the current user session or token

#### `GET /auth/me`

Purpose:

- return the current authenticated identity
- used by the frontend to initialize the current user and permissions model

#### `GET /users`

Purpose:

- list users known to the system
- used for administration, assignment targets, and audit interpretation

#### `POST /users`

Purpose:

- create a new user
- may also generate an API key depending on workflow

#### `PATCH /users/:userId`

Purpose:

- update user metadata, role, or active state

#### `POST /users/:userId/api-keys`

Purpose:

- create a new API key for an agent or automation user

#### `GET /users/:userId/api-keys`

Purpose:

- list active API key records for operational review

#### `DELETE /users/:userId/api-keys/:keyId`

Purpose:

- revoke an API key

## 3. Projects

These endpoints manage the project index and project metadata.

The frontend `Projects` page depends on this area.

#### `GET /projects`

Purpose:

- list all visible projects
- powers the top-level `Projects` page
- should support light filtering and sorting later, but first implementation can be a simple list

#### `POST /projects`

Purpose:

- create a new project

#### `GET /projects/:projectId`

Purpose:

- return a single project's metadata and high-level summary

#### `PATCH /projects/:projectId`

Purpose:

- update project metadata such as title, slug, description, or state

#### `DELETE /projects/:projectId`

Purpose:

- archive or remove a project according to policy

## 4. Tasks

These endpoints are the core of the product.

They support board view, task detail view, task creation, updates, hierarchy, and copy-ID workflows.

#### `GET /projects/:projectId/tasks`

Purpose:

- list tasks within a project
- powers board view and task lists
- should support filtering by status, priority, assignee, label, and text query

#### `POST /projects/:projectId/tasks`

Purpose:

- create a task inside a project
- supports optional parent task relationship when creating a subtask

#### `GET /tasks/:taskId`

Purpose:

- fetch a full task detail view
- should include enough context for the Task Detail page:
  - task metadata
  - parent reference
  - subtasks
  - comments
  - recent activity

#### `PATCH /tasks/:taskId`

Purpose:

- update task fields such as title, description, status, priority, assignee, and labels

#### `POST /tasks/:taskId/subtasks`

Purpose:

- create one or more subtasks under an existing task

#### `POST /tasks/:taskId/reparent`

Purpose:

- change a task's parent relationship after creation
- this endpoint exists specifically because the earlier system had a gap here

#### `DELETE /tasks/:taskId`

Purpose:

- remove or archive a task according to policy

## 5. Task Comments

These endpoints handle discussion and incremental task notes.

#### `GET /tasks/:taskId/comments`

Purpose:

- list comments for a task
- used by task detail views and audit review

#### `POST /tasks/:taskId/comments`

Purpose:

- add a new comment to a task

#### `PATCH /comments/:commentId`

Purpose:

- edit an existing comment if policy allows it

#### `DELETE /comments/:commentId`

Purpose:

- remove or hide a comment if policy allows it

## 6. Sessions

In this product, Sessions are snapshot records for exported agent sessions.

Current reference type:

- `opencode`

These endpoints power the standalone `Sessions` page.

#### `GET /sessions`

Purpose:

- list stored session snapshots
- powers the Sessions registry page
- should return the minimal visible metadata only:
  - title
  - snapshot UUID
  - description
  - artifact name
  - artifact path

#### `POST /sessions`

Purpose:

- create a new session snapshot record
- used after uploading or registering a session export artifact

#### `GET /sessions/:snapshotId`

Purpose:

- fetch one session snapshot record by snapshot UUID

#### `PATCH /sessions/:snapshotId`

Purpose:

- update session snapshot metadata such as title, description, or artifact location

#### `DELETE /sessions/:snapshotId`

Purpose:

- remove a session snapshot record
- does not necessarily imply deleting the underlying artifact unless explicitly designed that way

#### `POST /sessions/uploads`

Purpose:

- accept a session export artifact upload
- return enough information to create or update a session snapshot record

This endpoint is for file ingestion. It should support backend-managed storage targets.

#### `GET /sessions/:snapshotId/download`

Purpose:

- provide a download response or redirect for the stored export artifact
- should work whether the artifact lives in local filesystem storage or S3-compatible storage

## 7. Activity

Activity is a first-class global timeline.

It is not only task detail history. It also powers the standalone `Activity` page.

#### `GET /activities`

Purpose:

- list activity timeline entries across the system or within visible scope
- powers the standalone Activity page
- must support:
  - filtering by project
  - filtering by task
  - filtering by label
  - combining multiple filters
  - `NOT` mode for those filters
  - ascending or descending sort by activity time

#### `GET /activities/:activityId`

Purpose:

- fetch a single activity record in detail if needed later

## 8. Search

Search is for cross-task discovery.

#### `GET /search`

Purpose:

- search tasks across visible scope
- should support full-text search against task title, description, comments, and labels
- should be usable by both frontend quick search and agent CLI

## 9. Webhooks

Webhooks provide event delivery to external systems.

#### `GET /webhooks`

Purpose:

- list webhook registrations

#### `POST /webhooks`

Purpose:

- create a webhook subscription

#### `PATCH /webhooks/:webhookId`

Purpose:

- update webhook metadata, secret, or event subscriptions

#### `DELETE /webhooks/:webhookId`

Purpose:

- remove a webhook registration

## 10. Recommended Activity Event Types

The backend should emit activity entries for at least these changes:

- task created
- task updated
- task status changed
- task assignee changed
- task comment added
- subtask created
- task reparented
- session snapshot created
- session snapshot updated
- session snapshot deleted

## 11. API Shape Guidance

These are not strict schema rules yet, but they should guide implementation.

### List endpoints

List endpoints should consistently support:

- pagination
- stable ordering
- machine-readable response structure

### Error behavior

Errors should be:

- explicit
- stable for CLI parsing
- easy for frontend to surface without guessing

### IDs

Use stable UUID-style IDs for system records.

Important distinction:

- task IDs are task identifiers
- session snapshot IDs are our own UUIDs
- artifact paths are storage locations, not primary IDs

## 12. Endpoint Priority For First Implementation

Recommended order:

1. health and auth basics
2. projects list/detail
3. tasks list/create/detail/update
4. comments
5. activity list with filters and sort
6. sessions snapshot registry and upload/download flow
7. search
8. webhooks

## 13. Current Frontend Dependencies

The current frontend mock implies these backend priorities:

### Home

- counts for projects, tasks, sessions, activities
- recent tasks
- recent sessions
- recent activities

### Projects

- list of projects with summary counts

### Board

- tasks grouped by status
- task metadata for cards

### Task Detail

- parent
- subtasks
- labels
- status
- priority
- copyable task ID

### Sessions

- list of snapshot records
- upload entry
- download action
- artifact name/path visibility

### Activity

- timeline list
- project/task/label filtering
- `NOT` filtering support
- ascending/descending sort control

## 14. Out Of Scope For This Document

This document intentionally does not lock down:

- exact request/response JSON schema
- exact database columns
- exact upload storage implementation
- exact authorization matrix per role
- exact OpenAPI component layout

Those should be defined after this functional surface is accepted.
