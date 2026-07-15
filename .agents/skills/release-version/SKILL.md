---
name: release-version
description: |
  Release a version in this repository with forgekit. Use when you need
  to bump chart, container, or binary versions, create the release commit, tag
  the app with <app-name>-v<semver>, and verify the tag-driven GitHub Actions
  release workflows.
metadata:
  forgekit-version: "0.6.1"
---

# Release Version Skill

Use this skill for the repository-wide release flow.

This repository uses:

- `forgekit v0.6.1`
- tag-driven release workflows
- `version-control.yaml` as the single app registry
- tags in the form `<app-name>-v<semver>`

## Core Rules

- Do not edit version fields manually when `forgekit` owns them.
- Use `forgekit` to bump chart, container, or binary versions.
- Release is triggered by pushing a tag, not by manually dispatching workflows.
- For chart apps, linked containers and linked binaries are resolved from `Chart.yaml` annotations.
- `version-control.yaml` is the source of truth for app registration.
- When `setup/forgekit.sh` changes its default forgekit version, update
  `metadata.forgekit-version` and review this skill against the new CLI behavior.

## Repo Entry Points

```bash
bash ./setup/forgekit.sh
build/bin/gh
```

Typical local setup:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
build/bin/gh --version
```

## Local Lint

Run the same forgekit lint configuration used by GitHub Actions from any
directory inside the repository:

```bash
PROJECT_ROOT="$(git rev-parse --show-toplevel)"
FORGEKIT_BIN="$(bash "$PROJECT_ROOT/setup/forgekit.sh")"
"$FORGEKIT_BIN" --project-root "$PROJECT_ROOT" lint \
  --config "$PROJECT_ROOT/lint.yaml"
```

This executes every command declared in `lint.yaml`, including repository test
commands. When operating as Codex, ask for permission before running it.

## Release Flow

### 1. Identify the release target

Confirm the app name from `version-control.yaml`.

Examples:

- `codespace`
- `aria2`
- `sshpiper`

Check the resolved app metadata:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version get <app-name> --output json
```

Use this to verify:

- app type
- chart path
- linked container targets
- linked binary targets
- current versions

### 2. Decide which artifacts changed

Do not bump everything by default.

Typical rule:

- chart template or values change: bump chart
- container runtime change: bump the affected container
- CLI or shipped binary change: bump the affected binary
- when chart values must track linked artifact versions: bump chart with `--sync`

Example: only the frontend runtime changed in `aria2`:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump container aria2-frontend patch
"$FORGEKIT_BIN" version bump chart aria2 patch --sync
```

That updates:

- `application/aria2/container/aria-ng/VERSION`
- chart version
- synced image tags in chart values

Example: chart-only fix:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump chart aria2 patch --sync
```

Example: binary change for a linked CLI target:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump binary <binary-name> patch
"$FORGEKIT_BIN" version bump chart <app-name> patch --sync
```

### 3. Review the bumped files

Inspect only the intended version changes before committing.

```bash
git status --short
git diff
```

Verify that:

- only the expected version files changed
- chart `version` changed when a chart release is needed
- linked image tags or binary references were synced when expected
- untouched artifacts kept their existing versions

### 4. Commit the version bump

Use a commit message that matches the repository style.

Typical examples:

```bash
git commit -m "chore(aria2): bump frontend release versions"
git commit -m "chore(aria2): bump release versions"
git commit -m "fix(aria2): correct chart image repository"
```

Then push the branch:

```bash
git push
```

### 5. Wait for lint on the release commit

Pull requests and pushes to `main` trigger `lint`. A push to another branch
without a pull request does not trigger it.

Do not create the release tag until the release commit is on `main` and its
`lint` run has succeeded.

Monitor the latest `lint` run for the pushed commit:

```bash
build/bin/gh run list --limit 12 \
  --json databaseId,workflowName,headSha,status,conclusion,url \
  --jq '.[] | select(.workflowName == "lint")'
```

Watch the run to completion:

```bash
build/bin/gh run watch <lint-run-id> --interval 10 --exit-status
```

### 6. Create and push the release tag

The tag format is mandatory:

```bash
git tag "<app-name>-v<semver>"
git push origin "<app-name>-v<semver>"
```

Example:

```bash
git tag "aria2-v1.1.2"
git push origin "aria2-v1.1.2"
```

This triggers:

- `release-chart`
- `release-container`
- `release-binary`

Each workflow decides whether it should actually publish anything.

### 7. Monitor release workflows after tag

List runs for the tag:

```bash
build/bin/gh run list --limit 12 \
  --json databaseId,workflowName,displayTitle,event,status,conclusion,headBranch,url \
  --jq '.[] | select(.headBranch == "<app-name>-v<semver>")'
```

Watch a run to completion:

```bash
build/bin/gh run watch <run-id> --interval 10 --exit-status
```

Expected workflows:

- `release-chart`
- `release-container`
- `release-binary`

Important: a workflow may run even if only part of the app changed. For chart apps, linked targets are resolved from the chart metadata, so a container release run can still include unchanged linked targets.

### 8. Record the release result

After success, record:

- pushed tag
- workflow run ids
- released chart version
- released container version(s)
- released binary version(s)

If this release corresponds to a tracked requirement, add a task comment with the shipped versions.

Example:

```text
Released in chart version 0.1.4 with frontend container version 0.1.3.
```

## Typical Scenarios

### Frontend-only change in a chart app

Use this when only a linked frontend container changed.

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump container aria2-frontend patch
"$FORGEKIT_BIN" version bump chart aria2 patch --sync
git add application/aria2/container/aria-ng/VERSION \
  application/aria2/chart/Chart.yaml \
  application/aria2/chart/values.yaml
git commit -m "chore(aria2): bump frontend release versions"
git push
build/bin/gh run watch <lint-run-id> --interval 10 --exit-status
git tag "aria2-v1.1.2"
git push origin "aria2-v1.1.2"
```

### Chart-only fix

Use this when templates or values changed but linked runtime artifacts did not.

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump chart aria2 patch --sync
```

### Multi-artifact app release

Use this when one app owns a chart plus linked containers or binaries.

Process:

1. Bump only the changed linked artifacts.
2. Bump the chart with `--sync`.
3. Commit and push.
4. Tag `<app-name>-v<semver>`.
5. Monitor all triggered release workflows.

## Files To Know

- `version-control.yaml`
- `.github/scripts/release-plan.sh`
- `.github/workflows/lint.yaml`
- `.github/workflows/release-chart.yaml`
- `.github/workflows/release-container.yaml`
- `.github/workflows/release-binary.yaml`

## Notes

- `main` pushes validate but do not release.
- `lint` and `release` are separate phases: push first, wait for `lint`, then tag.
- This repository does not use `workflow_dispatch` for releases.
- Container build context is the repository root by default. The
  `codespace-runtime` target uses its container directory as an explicit
  workflow override.
- App names must be globally unique and must not contain `/`.
- Binary packaging is still repository workflow logic even though version ownership is managed through `forgekit`.
