# agent-task-manager

`agent-task-manager` is the Go-based rebuild of the current `pith` task system.

## Layout

- `chart/`: Helm chart
- `backend/`: Go API
- `cli/`: Go CLI client
- `frontend/`: web UI
- `design.md`: implementation design

## Status

This directory now includes an initial backend implementation with a Bun-based switchable SQL storage layer.

## Backend Storage Modes

The backend is planned around Bun with two database modes behind one compatibility layer:

- `sqlite`: default local persistent database stored under `ATM_DATA_DIR`
- `postgres`: external PostgreSQL using a normal connection string

The current implementation uses Bun with the pure-Go `modernc.org/sqlite` driver for SQLite, so the backend can keep using the existing non-CGO container build path while still supporting PostgreSQL by configuration.

### Backend environment

- `ATM_API_ADDR`: API listen address, default `:8080`
- `ATM_DATABASE_DRIVER`: `sqlite` or `postgres`, default `sqlite`
- `ATM_DATABASE_DSN`: optional explicit DSN
- `ATM_DATA_DIR`: local data root for sqlite and artifacts, default `/var/lib/agent-task-manager`
- `ATM_ARTIFACTS_DIR`: upload storage path for session artifacts
- `ATM_AUTH_MODE`: `disabled` or `apikey`, default `disabled`

### Example sqlite setup

```bash
ATM_DATABASE_DRIVER=sqlite \
ATM_DATA_DIR=/data/agent-task-manager \
go run ./backend/src/cmd/agent-task-manager-api
```

### Example postgres setup

```bash
ATM_DATABASE_DRIVER=postgres \
ATM_DATABASE_DSN='postgres://user:pass@postgres:5432/agent_task_manager?sslmode=disable' \
go run ./backend/src/cmd/agent-task-manager-api
```
