# CI/CD Overview

This repository should use a tag-driven release model built on `forgekit v0.4.1`.

## Core Rules

- `main` push validates but does not release.
- Release is triggered by tags of the form `<app-name>-v<semver>`.
- `version-control.yaml` is the single app registry.
- Charted apps own their chart-defined images.
- Standalone image apps are registered directly in `version-control.yaml` and represent one image each.
- Binary apps are registered directly in `version-control.yaml` and are independent apps.
- Workflows are thin callers over shared release logic.

## Strategy Docs

- `version-control-strategy.md`: how code-side apps map into `version-control.yaml`
- `release-trigger-strategy.md`: shared tag-trigger and cascade logic for workflows

## Workflow Shape

- `lint.yaml` defines repo quality policy for `forgekit lint`
- `release-chart.yaml` publishes charted apps
- `release-image.yaml` publishes charted and standalone image apps
- `release-binary.yaml` publishes binary apps
