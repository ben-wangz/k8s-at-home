---
name: update-sub2api
description: Detect the latest stable Wei-Shaw/sub2api release, compare it with the local sub2api Helm chart, update the image and chart versions through forgekit, and optionally publish through the repository release flow. Use when asked whether the sub2api chart is current, to update it to the latest upstream version, or to release a new sub2api chart.
---

# Update sub2api

Update the repository's `sub2api` chart from the latest stable upstream GitHub release. Keep version detection read-only, let `forgekit` own tracked version changes, and delegate publishing to `release-version`.

## Required skills

- Use `gh-cli` for GitHub authentication, API queries, and Actions monitoring.
- Use `release-version` for lint, commit, push, tag, and workflow verification.
- Read each required skill completely before performing its part of the workflow.

## Workflow

### 1. Determine scope

Distinguish among check-only, update-only, and update-and-release requests. Do not commit, push, or tag unless the user explicitly requests publishing or separately approves those actions.

### 2. Run preflight checks

1. Resolve the project root with `git rev-parse --show-toplevel`.
2. Inspect `git status --short --branch` and preserve unrelated changes.
3. Confirm `sub2api` remains registered in `version-control.yaml`:

   ```bash
   PROJECT_ROOT="$(git rev-parse --show-toplevel)"
   FORGEKIT_BIN="$(bash "$PROJECT_ROOT/setup/forgekit.sh")"
   "$FORGEKIT_BIN" --project-root "$PROJECT_ROOT" version get sub2api --output json
   ```

4. Verify GitHub CLI authentication before querying upstream. If authentication is missing, start `gh auth login` and let the user complete device authorization.

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
- `local-inconsistent`: Stop and report that `Chart.yaml:appVersion` and `values.yaml:image.tag` disagree.

Compare upstream against both `appVersion` and `image.tag`, not against the chart package version. The chart `version` is an independent release counter.

### 4. Update with forgekit

`sub2api` uses an external image, so it intentionally has no permanent linked-container annotation. Forgekit 0.6.1 cannot directly set an arbitrary external version. Use this temporary sync source:

1. With `apply_patch`, add `.forgekit-tmp/sub2api/VERSION` containing exactly `upstreamVersion` without the leading `v`.
2. With `apply_patch`, temporarily add this annotation to `application/sub2api/chart/Chart.yaml`. If an `annotations` map exists in the future, merge only this key and preserve the other entries.

   ```yaml
   annotations:
     sub2api/images: |
       - name: sub2api-upstream
         path: .forgekit-tmp/sub2api
         valuesKey: image.tag
   ```

3. Run forgekit once:

   ```bash
   PROJECT_ROOT="$(git rev-parse --show-toplevel)"
   FORGEKIT_BIN="$(bash "$PROJECT_ROOT/setup/forgekit.sh")"
   "$FORGEKIT_BIN" --project-root "$PROJECT_ROOT" \
     version bump chart sub2api patch --sync
   ```

4. With `apply_patch`, remove only the temporary annotation and VERSION file. Remove the empty `.forgekit-tmp` directories afterward.
5. If forgekit fails, clean up the temporary annotation and file before reporting the failure.

Do not manually edit `Chart.yaml:version`, `Chart.yaml:appVersion`, or `values.yaml:image.tag`. Forgekit must produce all three tracked version changes.

### 5. Review the update

Run the detection script again and require `comparison: current`. Then review:

```bash
git status --short
git diff --check
git diff -- application/sub2api/chart/Chart.yaml \
  application/sub2api/chart/values.yaml
```

The final version diff must contain only:

- a patch bump to `Chart.yaml:version`;
- `Chart.yaml:appVersion` set to `upstreamVersion`;
- `values.yaml:image.tag` set to `upstreamVersion`.

Do not include `.forgekit-tmp` or unrelated worktree changes.

### 6. Test and publish

Ask before running the local forgekit lint because repository policy classifies it as a test command. After permission, run the lint command from `release-version`.

For an authorized release, follow `release-version` exactly:

1. Commit only the two sub2api chart files with a message such as `chore(sub2api): upgrade image to <upstreamVersion> and release <chartVersion>`.
2. Push the release commit to `main`.
3. Wait for that exact commit's `lint` run to succeed.
4. Create and push `sub2api-v<chartVersion>`. Use the chart package version, not the upstream application version.
5. Monitor `release-chart`, `release-container`, and `release-binary`. Container and binary publication should normally be skipped for this external-image chart.
6. Verify the published chart:

   ```bash
   helm show chart \
     oci://ghcr.io/ben-wangz/k8s-at-home-charts/sub2api \
     --version "<chartVersion>"
   ```

Record the release commit, tag, workflow run IDs, chart version, app/image version, and OCI digest.

## Guardrails

- Use GitHub's `releases/latest` endpoint so drafts and prereleases are excluded.
- Normalize only one leading `v` and require strict `major.minor.patch` SemVer.
- Never downgrade automatically.
- Never bump the chart when upstream and local application versions already match.
- Never persist a linked-container annotation for the external upstream image.
- Never overwrite, stage, or commit unrelated user changes.
