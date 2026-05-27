---
name: agent-task-manager-cli
description: |
  Work with agent-task-manager through the local atmctl CLI. Use when you need
  to list, read, create, or update projects, tasks, sessions, or activities
  from this repository without going through the UI.
compatibility: opencode
---

# Agent Task Manager CLI Skill

Use the local wrapper when available:

```bash
build/bin/atmctl
```

Or run from source:

```bash
go -C application/agent-task-manager/cli/src run ./cmd/atmctl
```

## Environment

```bash
export ATM_API_URL="http://host:8080/api/v1"
export ATM_API_KEY="..."   # optional
export ATM_OUTPUT=raw       # optional, for automation
```

## Common Usage

```bash
build/bin/atmctl projects list
build/bin/atmctl projects create -slug <slug> -title <title>
build/bin/atmctl tasks get <task-id>
build/bin/atmctl tasks update <task-id> -status in_progress
build/bin/atmctl tasks comment <task-id> -body "note"
```

## References

- `references/requirement-authoring.md`: how to write a requirement so it is clear enough to implement, review, and release.
- `references/requirement-development.md`: how to take a requirement from intake through implementation, validation, review, release handoff, and completion.

## Notes

- Prefer `tasks get` when you already know the task id.
- Use single-dash flags like `-status`, `-project`, `-q`.
- The CLI is self-explanatory: run `build/bin/atmctl` or `build/bin/atmctl <group>` for usage.
- Use comments to record implementation summaries, validation results, and released version numbers.
