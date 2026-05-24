# Version Control Strategy

## Goal

Define how code-side apps map into `version-control.yaml` app entries.

## Rules

1. `version-control.yaml` is the single app registry.
2. App names must be unique across charted apps, standalone image apps, and binary apps.
3. A charted app owns its chart and its chart-defined images.
4. A standalone image app has no chart and represents exactly one image.
5. A binary app is independent and is not coupled to a chart.

## Charted Apps

Use a chart entry when the app has a Helm chart.

For charted apps:

- the chart path is listed in `version-control.yaml`
- container images belong to the chart definition
- those images must not be duplicated under the same app name in `version-control.yaml`

Example:

- `agent-task-manager` has a chart
- its backend and frontend images stay in the chart
- `version-control.yaml` only registers the charted app name

## Standalone Image Apps

Use a direct image app entry when the app has no chart.

For standalone image apps:

- `name` is the unique app name
- `path` points to the `container/` directory
- `versionFile` lives under the same `container/` directory
- one app corresponds to one image only

Example:

- `podman-in-container`

## Binary Apps

Use a binary entry when the app is released as a standalone binary.

For binary apps:

- `name` is the unique app name
- `path` points to the binary source root
- `versionFile` lives at that app's version file

Example:

- `atmctl`

## Resulting Model

`version-control.yaml` should explicitly list:

- charted apps
- standalone image apps
- binary apps
