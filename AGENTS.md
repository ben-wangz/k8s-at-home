# k8s-at-home Codex Rules

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

## Codex Workflow
- Keep responses concise and token-efficient.
- Use `rg` for file and text searches when available.
- Use `apply_patch` for focused manual edits.

## Testing
- Ask before running tests.

## Commit Messages
- Commit messages should describe only the code/doc changes.
- Do not include unrelated metadata.
- Never create a commit or push changes without explicit user approval.

## Version Management
- Only use `forgekit` to modify chart, image, or module version numbers.
- Do not manually edit version fields such as `Chart.yaml` `version`, `appVersion`, `values.yaml` image tags, or `container/VERSION` files.
- When a chart version bump should also update image references, use the appropriate `forgekit` sync flow instead of manual edits.

## File Operations
- Prefer `mv` over create-then-delete for rename/move operations.
