---
name: update-sub2api
description: Detect the latest stable Wei-Shaw/sub2api release and update only the official image tag in the local sub2api Helm values. Use when asked whether the sub2api image is current or to update it; delegate chart version bumps and releases to release-version.
---

# Update sub2api

Update the external `ghcr.io/wei-shaw/sub2api` image reference from the latest stable upstream GitHub release. The image update must leave exactly one tracked version change: `application/sub2api/chart/values.yaml:image.tag`.

Do not change `Chart.yaml:version` or `Chart.yaml:appVersion` as part of an upstream image update. Chart packaging and publication are separate operations owned by `release-version`.

## Required skills

- Always read and use `gh-cli` before GitHub authentication or API queries.
- When the user requests a chart version bump or publication, read and use `release-version` for that entire phase.
- Read each required skill completely before performing its part of the workflow.

## Workflow

### 1. Determine scope

Distinguish among:

- check-only: detect and report the official image version;
- image update: update only `values.yaml:image.tag`;
- chart bump or release: finish the image update, then hand control to `release-version`.

Never infer permission to commit, push, tag, or publish from an image update request.

### 2. Run preflight checks

1. Resolve the project root with `git rev-parse --show-toplevel`.
2. Inspect `git status --short --branch` and preserve unrelated changes.
3. Record the existing diffs for `Chart.yaml` and `values.yaml` so cleanup can preserve user changes.
4. Confirm `sub2api` remains registered in `version-control.yaml`:

   ```bash
   PROJECT_ROOT="$(git rev-parse --show-toplevel)"
   FORGEKIT_BIN="$(bash "$PROJECT_ROOT/setup/forgekit.sh")"
   "$FORGEKIT_BIN" --project-root "$PROJECT_ROOT" version get sub2api --output json
   ```

5. Verify GitHub CLI authentication before querying upstream. If authentication is missing, start `gh auth login` and let the user complete device authorization.

### 3. Detect the latest version

Invoke the bundled script by absolute path so it does not depend on the current working directory:

```bash
PROJECT_ROOT="$(git rev-parse --show-toplevel)"
"$PROJECT_ROOT/.agents/skills/update-sub2api/scripts/check-version.sh"
```

Interpret `comparison` as follows:

- `current`: Report that no update is needed and stop without changing files.
- `upstream-newer`: Continue with `upstreamVersion` as the forgekit sync input.
- `local-newer`: Refuse an automatic downgrade and report both versions.

Compare upstream only against `values.yaml:image.tag`. Do not use either `Chart.yaml` version field to decide whether the official image needs an update.

### 4. Update only the image with forgekit

`sub2api` uses an external image and intentionally has no permanent linked-container annotation. Forgekit 0.6.1 cannot directly set an arbitrary external version, and `version sync` also rewrites `Chart.yaml:appVersion`. Use a temporary sync source, then restore that incidental Chart change:

1. Record the exact original `Chart.yaml:appVersion` value.
2. With `apply_patch`, add `.forgekit-tmp/sub2api/VERSION` containing exactly `upstreamVersion` without the leading `v`.
3. With `apply_patch`, temporarily add this annotation to `application/sub2api/chart/Chart.yaml`. If an `annotations` map exists, merge only this key and preserve all other entries.

   ```yaml
   annotations:
     sub2api/images: |
       - name: sub2api-upstream
         path: .forgekit-tmp/sub2api
         valuesKey: image.tag
   ```

4. Run forgekit sync, not a chart bump:

   ```bash
   PROJECT_ROOT="$(git rev-parse --show-toplevel)"
   FORGEKIT_BIN="$(bash "$PROJECT_ROOT/setup/forgekit.sh")"
   "$FORGEKIT_BIN" --project-root "$PROJECT_ROOT" \
     version sync sub2api
   ```

5. With `apply_patch`, restore `Chart.yaml:appVersion` to its exact original value, remove only the temporary annotation, and delete the temporary VERSION file. Remove empty `.forgekit-tmp` directories afterward.
6. Confirm the post-cleanup `Chart.yaml` diff exactly matches its preflight diff. This protects both version fields and any existing user changes.
7. If forgekit fails, restore the original `appVersion` if needed and clean up the temporary annotation and file before reporting the failure.

Forgekit must produce the image tag update. Restoring its incidental `appVersion` rewrite is cleanup, not a chart version update.

### 5. Review the update

Run the detection script again and require `comparison: current`. Then review:

```bash
git status --short
git diff --check
git diff -- application/sub2api/chart/Chart.yaml \
  application/sub2api/chart/values.yaml
```

The new diff introduced by this workflow must contain only `values.yaml:image.tag` set to `upstreamVersion`. `Chart.yaml:version` and `Chart.yaml:appVersion` must remain unchanged.

Do not include `.forgekit-tmp` or unrelated worktree changes.

### 6. Delegate chart bump and release

If the user requests a chart version bump, lint, commit, push, tag, or publication, stop following this workflow after reviewing the image diff. Read `release-version` completely and follow it for all remaining actions, including test permission and release verification.

## Guardrails

- Use GitHub's `releases/latest` endpoint so drafts and prereleases are excluded.
- Normalize only one leading `v` and require strict `major.minor.patch` SemVer.
- Never downgrade automatically.
- Never bump `Chart.yaml:version` during an upstream image update.
- Never persist an `appVersion` change caused by image synchronization.
- Never persist a linked-container annotation for the external upstream image.
- Never overwrite, stage, or commit unrelated user changes.
