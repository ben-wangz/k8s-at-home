# CI/CD Upgrade Proposal

## Current Assessment

The current repository already uses `forgekit` for version and publish operations, but the CI/CD design is still closer to a direct build pipeline than to a release-driven pipeline.

Current characteristics:

- `publish-chart.yaml` publishes charts on `main` push or manual dispatch.
- `publish-image.yaml` publishes images on `main` push or manual dispatch.
- Release targets are hardcoded in workflow matrices.
- `version-control.yaml` only registers charts.
- `setup/forgekit.sh` was pinned to `0.3.1`.
- `agent-task-manager` already has chart and image version metadata, but is missing from `version-control.yaml` and current publish workflows.

This creates three practical problems:

1. Release intent is implicit: merging to `main` can publish artifacts.
2. Repository metadata is incomplete: workflows and manifest can drift.
3. CI is underusing newer `forgekit` capabilities that can centralize lint and app registry truth.

## Forgekit Baseline

This repository should move to `forgekit v0.4.1` as the minimum and preferred baseline.

Why `0.4.1` matters for this repository:

- `forgekit lint` is now clearly built around a repo-level `lint.yaml`.
- `forgekit version` supports a richer manifest model, including `binaries`.
- `publish` semantics are clearer around `--semver`, `--multi-tag`, clean-worktree requirements, and registry configuration.
- Integration guidance now explicitly recommends absolute-path invocation and explicit `--project-root`, which fits this repository's workflow style.

## Relevant Forgekit 0.4.1 Capabilities

### 1. `forgekit lint`

`forgekit lint` now expects repository policy to live in `lint.yaml` at the git root.

Implication for this repository:

- CI policy should move out of workflow shell snippets into `lint.yaml` where possible.
- The GitHub workflow should become a thin wrapper around `forgekit lint`.
- Repo-level conventions such as formatting, shell checks, YAML checks, Helm checks, or selected test commands can be centralized.

### 2. `forgekit publish ... --semver --multi-tag`

`forgekit publish` now has clearer semantics:

- default publish uses git-version
- `--semver` requires clean version state
- `--multi-tag` is only valid with `--semver --push`
- prerelease and `0.x.y` versions degrade multi-tag behavior
- versions with `+build` metadata are invalid for OCI tags

Implication:

- Formal release workflows should use `--semver`.
- Tag-triggered release is the natural match for `--semver` publishing.
- Main-branch CI should avoid pushing artifacts and should focus on validation builds.

### 3. `binaries` support in `version-control.yaml`

`forgekit` now supports explicit binary version declarations.

This is not only for future growth in this repository. There is already a concrete binary release target:

- `application/agent-task-manager/cli/` should be released as a standalone CLI binary
- its current layout already fits this model well: separate `go.mod`, separate `cmd/atmctl`, separate component boundary

Implication:

- `version-control.yaml` should include a `binaries` section for `atmctl`
- the CLI should have a dedicated version file, for example `application/agent-task-manager/cli/VERSION`
- binary release should follow the same `version-truth + tag-anchor` contract as charts and images

### 4. Recommended integration model

The upstream integration docs strongly recommend:

- downloading a known release binary
- verifying checksums
- invoking via absolute path
- always passing `--project-root`

Implication:

- current `setup/forgekit.sh` is still a good fit
- workflows should standardize on `"${FORGEKIT_BIN}" --project-root "$GITHUB_WORKSPACE" ...`
- all repo automation should avoid assuming the current directory

## Proposed Release Model

The repository should move to a `version-truth + tag-anchor` model, aligned with how modern `forgekit` is intended to be used.

Core rules:

1. Version truth lives in repository files.
2. `forgekit` is the only supported mutator for chart/image/binary versions.
3. `main` push validates but does not release.
4. Git tags trigger release workflows.
5. The same app tag should trigger every artifact workflow for that app.
6. Workflows that do not apply to the tagged app should remain effectively no-op.

This keeps the same philosophy already documented by `forgekit` itself:

- repository files define the version
- tags are release anchors for one app release event

## Recommended Repository Metadata Changes

### 1. Expand `version-control.yaml`

Today it only contains `charts`. It should become the single app registry managed by `forgekit` workflows.

Recommended direction:

- keep `charts` as the source of truth for chart paths
- ensure every releasable chart is included, including `agent-task-manager`
- add `binaries` for standalone apps, starting with `atmctl`
- add direct container image app entries for standalone image apps without charts
- if a charted app has container images, they belong to the chart and must not be duplicated as the same app name in `version-control.yaml`
- if a same-named container image appears in `version-control.yaml`, treat it as a separate standalone app

Recommended binary entry:

- name: `atmctl`
- path: `application/agent-task-manager/cli`
- versionFile: `VERSION`

This implies a repo change that should be treated as part of the CI/CD upgrade:

- add `application/agent-task-manager/cli/VERSION`

For containers, use the chart definition when a chart exists.

Reason:

- chart-local release metadata already exists in `Chart.yaml`
- `forgekit get` can read chart-defined image relationships
- workflows remain thin callers over shared cascade logic

### 2. Treat chart annotations as first-class release metadata

The repository already has a strong pattern in `Chart.yaml` annotations mapping image modules to:

- module name
- version file path
- `values.yaml` key

That metadata is useful repository metadata, but it should not become a hard dependency of the release mechanism itself.

Practical use:

- document image relationships inside the chart
- support normal development workflows when maintainers want to update chart image tags
- optionally generate workflow matrices from these annotations in a helper script later

### 3. Centralize cascade rules in code

Release cascade rules should live in one shared code path, not inside individual GitHub Actions workflows.

The shared code should decide:

- which app the tag belongs to
- whether that app has a chart release target
- whether that app has one or more container image release targets
- whether that app has a binary release target
- whether a workflow should do work or exit immediately

Workflow responsibility:

- parse the tag
- call the shared cascade logic
- run only the publish/build steps the shared logic says apply

This keeps the release topology consistent while letting each workflow remain independent.

## Recommended Workflow Topology

### 1. `lint.yaml`

Add a repository-level `lint.yaml` consumed by `forgekit lint`.

This should become the central place for quality policy, while GitHub Actions only runs it.

Recommended checks in `lint.yaml`:

- formatting checks for shell and Go where applicable
- selected static checks for repository scripts
- chart validation commands such as `helm lint` for managed charts
- optional smoke checks for container build metadata or release scripts

### 2. CI workflow

There is no need for a separate `ci.yaml` config file managed by the repository.

Recommended CI shape:

- keep CI policy in GitHub Actions workflows plus `lint.yaml`
- use `lint.yaml` only for checks that naturally belong to `forgekit lint`
- keep release validation logic in the release workflows themselves

Trigger:

- `pull_request`
- `push` to `main`

Responsibilities:

- checkout
- install `forgekit` via `setup/forgekit.sh`
- run `forgekit lint`
- run `forgekit version get`
- optionally run build validation for selected charts, images, and binaries without push

Important rule:

- CI should validate publishability but must not push release artifacts.

### 4. `release-chart.yaml`

Trigger:

- `push.tags` matching `<app-name>-v*`

Recommended trigger model:

- chart workflow, image workflow, and binary workflow should all listen to the same app tag pattern
- each workflow is independent and only releases the artifact types it owns
- if the tagged app has no chart, the chart workflow exits immediately

Recommended tag format:

- `<app-name>-v<semver>`

Examples:

- `aria2-v1.1.2`
- `chromium-bridge-v0.2.1`
- `agent-task-manager-v0.1.0`

Responsibilities:

1. Parse tag and resolve the app name.
2. Call the shared cascade logic.
3. If the app has no chart, exit successfully.
4. Publish the chart artifact for that app with:

`forgekit publish chart build --push --semver --multi-tag`

Why this fits `forgekit 0.4.1`:

- semver release is explicit
- multi-tag behavior is standardized by the tool
- one app tag can release the chart together with other app artifacts

### 5. `release-image.yaml`

Trigger:

- `push.tags` matching `<app-name>-v*`

Recommended trigger model:

- use the same `<app-name>-v<semver>` tag as the chart and binary workflows
- if the tagged app has no image targets, the image workflow exits immediately

Examples:

- `aria2-v1.1.2`
- `chromium-bridge-v0.2.1`
- `agent-task-manager-v0.1.0`

Responsibilities:

1. Parse tag and resolve the app name.
2. Call the shared cascade logic.
3. If the app has a chart, use `forgekit get` against the chart definition to resolve image targets.
4. If the app has no chart, resolve image targets from `version-control.yaml`.
5. If the app has no internal images, exit successfully.
6. Publish each image artifact for that app with:

`forgekit publish container build --push --semver --multi-tag`

### 6. `release-binary.yaml`

Trigger:

- `push.tags` matching `<app-name>-v*`

Recommended trigger model:

- use the same `<app-name>-v<semver>` tag as the chart and image workflows
- binary release is treated as an independent app trigger, not a chart-coupled cascade

Examples:

- `agent-task-manager-v0.1.0`

Responsibilities:

1. Parse tag and resolve the app name.
2. Call the shared cascade logic.
3. If the tag is not for a binary app, exit successfully.
4. Build multi-platform binaries.
5. Generate checksums.
6. Upload assets to GitHub Release.

For `atmctl`, the expected source layout is already present:

- module root: `application/agent-task-manager/cli/src`
- command entrypoint: `application/agent-task-manager/cli/src/cmd/atmctl`

This workflow should follow the same pattern used in `bot-cli`:

- app-based tag parsing
- multi-platform Go builds
- checksum generation
- GitHub Release asset upload

## Recommended Version Flow

Version bumps happen during development, not in release workflows.

### Image release flow

1. Change container code.
2. Run `forgekit version bump <module> <part>`.
3. Commit version changes.
4. Push the app tag to trigger image release together with the app's other artifacts.

### Binary release flow

1. Change CLI code.
2. Run `forgekit version bump atmctl <part>` once the binary is declared in `version-control.yaml`.
3. Commit version changes.
4. Push the binary app tag to trigger its independent release.

### Chart release flow

1. Change chart code.
2. Run `forgekit version bump-chart <chart> <part>` when the chart itself should release.
3. Commit version changes.
4. Push the app tag to trigger chart release together with the app's other artifacts.

## How Forgekit Should Influence the Final Design

After exploring `forgekit 0.4.1`, the most important adjustment to the earlier plan is this:

The design should not merely copy `bot-cli`'s tag-triggered release. It should use `forgekit` as the release contract engine.

That means:

- `lint.yaml` should define repository quality policy
- `version-control.yaml` should define chart inventory
- `version-control.yaml` should also define binary inventory
- app tags should be the shared trigger across independent artifact workflows
- cascade rules should live in shared code, not in workflow YAML
- chart-defined images should be resolved from the chart when a chart exists
- binaries should be treated as independent app triggers when they do not belong to a charted app
- `forgekit publish ... --semver --multi-tag` should be the canonical release action
- tags should trigger release only after file-based version truth is already committed

In short, `bot-cli` provides the release-trigger pattern, but `forgekit 0.4.1` provides the stronger foundation for release consistency.

## Direct Cutover Plan

This change does not need a staged migration or compatibility layer. The repository should switch directly to the new model.

Target end state:

1. `main` push never publishes artifacts.
2. `workflow_dispatch` is removed from release workflows.
3. Release only happens on tag push.
4. Chart, image, and binary release workflows are split but share the same app tag where applicable.
5. Each workflow releases only the artifacts it owns.
6. Workflows with no matching artifact for the tagged app exit immediately.
7. Repo quality policy that fits `forgekit lint` lives in `lint.yaml` and is executed through `forgekit lint`.
8. All releasable charts and binaries are declared in repository metadata, including `agent-task-manager`.

Concrete changes:

1. Upgrade `setup/forgekit.sh` to `v0.4.1`.
2. Add `lint.yaml` at repo root.
3. Update `version-control.yaml` so every managed chart is registered and `atmctl` is registered under `binaries`.
4. Add `application/agent-task-manager/cli/VERSION`.
5. Delete `workflow_dispatch` and `push.branches` release triggers from current publish workflows.
6. Replace current publish workflows with tag-triggered `release-chart.yaml`, `release-image.yaml`, and `release-binary.yaml`, all listening to the same app tag format where applicable.
7. Remove hardcoded `main`-push publishing behavior entirely.
8. Put cascade resolution in shared code and keep workflows as thin callers.
9. Standardize all workflow invocations on `"${FORGEKIT_BIN}" --project-root "$GITHUB_WORKSPACE" ...`.

What should not be kept:

- no compatibility bridge between old and new workflows
- no long transition period
- no manual dispatch release path
- no dual release mechanism where both `main` push and tag push can publish
- no cascade logic duplicated inside workflow YAML

## Open Design Decisions

These still need discussion before implementation:

1. Should CI perform full build validation for every chart/image/binary on every `main` push, or use targeted validation only?

## Recommendation Summary

The best next design for this repository is:

1. upgrade to `forgekit v0.4.1`
2. add repo-level `lint.yaml` for `forgekit lint`
3. register `atmctl` as a binary in `version-control.yaml` and add its `VERSION` file
4. convert release to tag-triggered workflows for chart, image, and binary artifacts
5. use the same app tag to trigger every artifact workflow for that app
6. remove `workflow_dispatch` and `main`-push publishing entirely
7. reduce workflow hardcoding by leaning on chart definitions and `version-control.yaml`

This approach is more robust than the current `main`-push publishing model and makes better use of `forgekit` than a plain copy of `bot-cli` would.
