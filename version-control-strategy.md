# Version Control Strategy

## Goal

Define how code-side apps map into `version-control.yaml` app entries.

## Rules

1. `version-control.yaml` is the single app registry.
2. `version-control.yaml` uses a unified `apps:` list.
3. App names must be unique across chart apps, container apps, and binary apps.
4. A chart app owns its chart and its chart-defined linked targets.
5. A standalone binary or standalone container app is independent and is not coupled to a chart.

## Chart Apps

Use a chart entry when the app has a Helm chart.

For chart apps:

- the chart path is listed in `version-control.yaml`
- linked containers and linked binaries belong to the chart definition
- those linked targets are declared in `Chart.yaml` annotations
- those linked targets must not be duplicated as standalone apps under the same app name in `version-control.yaml`

Example:

- `agent-task-manager` has a chart
- its backend and frontend images stay in the chart
- its `atmctl` binary can also stay linked from the chart
- `version-control.yaml` only registers the charted app name

## Standalone Container Apps

This repository does not currently have standalone container apps.

If standalone container apps are added later:

- `type` should be `container`
- `name` should be the unique app name
- `path` should point to the container directory
- `versionFile` defaults to `VERSION` when omitted

## Binary Apps

Use a binary entry when the app is released as a standalone binary.

For binary apps:

- `name` is the unique app name
- `path` points to the binary source root
- `versionFile` lives at that app's version file

## Resulting Model

`version-control.yaml` should explicitly list:

- chart apps
- standalone container apps when they exist
- binary apps
