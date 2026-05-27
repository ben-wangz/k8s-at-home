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
build/bin/atmctl tasks get <task-id>
build/bin/atmctl tasks update <task-id> -status in_progress
build/bin/atmctl tasks comment <task-id> -body "note"
```

## Requirement Flow

Use `agent-task-manager` to track a requirement from intake to post-release follow-up.

### 1. Create or find the project

List projects:

```bash
build/bin/atmctl projects list
```

If the target project does not exist yet, create it from `atmctl`:

```bash
build/bin/atmctl projects create \
  -slug agent-task-manager \
  -title agent-task-manager \
  -description "https://github.com/ben-wangz/k8s-at-home/tree/main/application/agent-task-manager" \
  -state active
```

### 2. Create the requirement task

```bash
build/bin/atmctl tasks create \
  -project <project-id> \
  -title "Add refresh controls to task page and evaluate other pages" \
  -description "Describe the user problem, expected UX, and evaluation scope." \
  -labels "agent-task-manager,frontend,ux"
```

New requirements should normally start as `backlog`.

### 3. Move to `in_progress` when implementation starts

When active development begins, move the task to `in_progress`.

```bash
build/bin/atmctl tasks update <task-id> -status in_progress
```

Add comments as work progresses when they help capture decisions, tradeoffs, or partial findings.

```bash
build/bin/atmctl tasks comment <task-id> -body "Implemented the first pass of the refresh control on the task page."
```

### 4. Move to `in_review` when development is done

When the code change is complete and ready for validation or release, move the task to `in_review`.

```bash
build/bin/atmctl tasks update <task-id> -status in_review
```

At this point, add a comment that summarizes what was implemented and what should be verified.

### 5. Treat release as the handoff point

Release is an external action and does not need to be performed through this skill, but it is a key workflow boundary.

- Before release: keep the task in `in_review`.
- After release succeeds: add a comment with the released versions that carry the change.
- If release fails or is rolled back: keep the task in `in_review` or move it back to `in_progress`, depending on whether more engineering work is required.

Example release note comment:

```bash
build/bin/atmctl tasks comment <task-id> -body "Released in chart version 0.1.4 with frontend container version 0.1.3."
```

### 6. Move to `done` after successful release and confirmation

Once the relevant release is complete and the change is confirmed in the shipped artifact or deployed environment, close the loop:

```bash
build/bin/atmctl tasks update <task-id> -status done
```

If follow-up work is discovered during review or after release, create a new task instead of overloading the original requirement.

## Status Guide

- `backlog`: requirement is captured but not being worked on yet.
- `in_progress`: implementation or investigation is actively underway.
- `in_review`: code is done and awaiting validation, release, or post-change confirmation.
- `done`: release succeeded and the requirement is confirmed complete.
- `cancelled`: requirement is intentionally dropped.

## Notes

- Prefer `tasks get` when you already know the task id.
- Use single-dash flags like `-status`, `-project`, `-q`.
- The CLI is self-explanatory: run `build/bin/atmctl` or `build/bin/atmctl <group>` for usage.
- Use comments to record implementation summaries and released version numbers.
