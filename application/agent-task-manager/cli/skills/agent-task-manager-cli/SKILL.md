# agent-task-manager-cli

Use this when you need to read or update data in `agent-task-manager` through the local CLI.

## Entry

- local wrapper: `build/bin/atmctl`
- source entry: `go -C application/agent-task-manager/cli/src run ./cmd/atmctl`

## Config

- `ATM_API_URL`: API base URL, usually ends with `/api/v1`
- `ATM_API_KEY`: optional bearer token
- `ATM_OUTPUT=raw`: single-line JSON for automation

## Common usage

```bash
ATM_API_URL="http://host:8080/api/v1" build/bin/atmctl projects list
ATM_API_URL="http://host:8080/api/v1" build/bin/atmctl tasks get <task-id>
ATM_API_URL="http://host:8080/api/v1" build/bin/atmctl tasks update <task-id> -status in_progress
ATM_API_URL="http://host:8080/api/v1" build/bin/atmctl tasks comment <task-id> -body "note"
```

## Notes

- prefer `tasks get` when you already know the task id
- use single-dash flags like `-status`, `-project`, `-q`
- if unsure, run `build/bin/atmctl` or `build/bin/atmctl <group>` for usage
