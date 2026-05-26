# CI/CD Overview

This repository uses a tag-driven release model built on `forgekit v0.6.0`.

## Core Rules

- `main` push validates but does not release.
- Release is triggered by tags of the form `<app-name>-v<semver>`.
- `version-control.yaml` is the single app registry.
- `version-control.yaml` uses a unified `apps:` list with `type: chart`, `type: container`, or `type: binary`.
- Chart apps own their chart-defined linked containers and linked binaries through `Chart.yaml` annotations.
- Standalone binaries are registered directly in `version-control.yaml` when they exist.
- Workflows are thin callers over `forgekit` commands plus minimal tag parsing.

## Forgekit 0.6.0 Model

`forgekit v0.6.0` provides the app resolution model needed by this repository:

- `forgekit version get <app-name>` resolves an app by name from `version-control.yaml`
- `forgekit version get <app-name> --output json` returns app type, version, and path metadata
- chart apps return linked targets from chart annotations
- linked chart targets include both containers and binaries when declared
- `forgekit publish chart build` publishes charts
- `forgekit publish container build` publishes containers

This means repository-local release logic only needs to parse tags and route resolved targets into the correct workflow.

## Release Contract

For a tag `<app-name>-v<semver>`:

- resolve `<app-name>` from the tag
- call `forgekit version get <app-name> --output json`
- determine whether the target app owns chart, container, or binary artifacts
- run only the matching workflow
- return no-op for non-applicable workflows

## Workflow Shape

- `lint.yaml` defines repo validation for `forgekit lint`
- `release-chart.yaml` publishes chart apps with `forgekit publish chart build`
- `release-container.yaml` publishes chart-linked containers with `forgekit publish container build`
- `release-binary.yaml` publishes chart-linked binaries or standalone binaries with repository-local packaging logic

## Notes

- `forgekit` does not currently publish binaries directly, so binary packaging remains repository workflow logic.
- Container targets use project root as the shared build context.
- With `forgekit 0.6.0`, app names must be globally unique and must not contain `/`.

## Strategy Docs

- `version-control-strategy.md`: how code-side apps map into `version-control.yaml`
- `release-trigger-strategy.md`: shared tag-trigger and cascade logic for workflows
