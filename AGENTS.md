# k8s-at-home Codex Rules

## Core Principles
- Be pragmatic over dogmatic.
- Keep single responsibility per function/class.
- Keep comments minimal; add comments only for non-obvious logic.
- After code changes, provide a minimal change overview (avoid long summaries).
- Do not create temporary summary files like `summary.md`.
- For large tasks, discuss a high-level design before implementation.
- Use UTF-8 by default.
- Project documentation must be written in Chinese, except documentation under `application/registry-hub/docs/`, which must be written in English. Keep AGENTS.md in English.

## Code Constraints
- Comments must be in English.
- Keep each function under 200 lines.
- Keep loop nesting to 3 levels or fewer.
- Shell scripts must be invokable via absolute path and must not depend on current working directory.

## Codex Workflow
- Keep responses concise and token-efficient.
- Use `rg` for file and text searches when available.
- Use `apply_patch` for focused manual edits.

## Commit Messages
- Commit messages should describe only the code/doc changes.
- Do not include unrelated metadata.
- Never create a commit or push changes without explicit user approval.

## Version Management
- Use `forgekit` to modify version numbers for artifacts built or released by this repository, including chart package versions, repository-owned image tags, and binary/module versions.
- Do not manually edit version fields for repository-owned artifacts, such as `Chart.yaml` `version`, linked repository-owned image tags, or `container/VERSION` files. Use `forgekit` for those fields.
- Third-party dependency versions are not repository artifact versions. Update their pins directly, including image tags in `values.yaml` and a third-party application's `Chart.yaml` `appVersion`; do not use `forgekit` or bump this repository's chart/module version solely because a third-party dependency version changes.
- When a chart version bump should also update linked repository-owned image references, use the appropriate `forgekit` sync flow instead of manual edits.

## File Operations
- Prefer `mv` over create-then-delete for rename/move operations.
