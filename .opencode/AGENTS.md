# k8s-at-home OpenCode Rules

## Core Principles
- Be pragmatic over dogmatic.
- Keep single responsibility per function/class.
- Keep comments minimal; add comments only for non-obvious logic.
- After code changes, provide a minimal change overview (avoid long summaries).
- Do not create temporary summary files like `summary.md`.
- For large tasks, discuss a high-level design before implementation.
- Use UTF-8 by default.

## Code Constraints
- Comments must be in English.
- Keep each function under 200 lines.
- Keep loop nesting to 3 levels or fewer.
- Shell scripts must be invokable via absolute path and must not depend on current working directory.

## OpenCode Workflow
- Keep responses concise and token-efficient.
- Prefer `Edit` for existing files.
- Use `Write` only when necessary (new file or full rewrite).
- If a single `Write` payload is large (roughly >200 lines or >2000 chars), split it into chunks.

## Testing
- Ask before running tests.

## Commit Messages
- Commit messages should describe only the code/doc changes.
- Do not include unrelated metadata.
- Never create a commit or push changes without explicit user approval.

## File Operations
- Prefer `mv` over create-then-delete for rename/move operations.
