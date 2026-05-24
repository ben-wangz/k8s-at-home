# Release Trigger Strategy

## Goal

Define the tag-driven logic shared by release workflows.

## Shared Model

1. A tag has the form `<app-name>-v<semver>`.
2. The tag identifies one app release event.
3. Each workflow is independent and only acts on the artifact type it owns.
4. Workflows that do not apply to the tagged app exit immediately.

## Shared Cascade Logic

The shared release logic should:

- parse the tag and resolve `app-name`
- look up the app in `version-control.yaml`
- decide whether the app has a chart release target
- decide whether the app has standalone image release targets
- decide whether the app has a binary release target
- return no-op when the app does not own that artifact type

## Chart Workflow

If the app has a chart:

- resolve chart-defined image relationships from the chart definition
- publish the chart artifact

If the app has no chart:

- exit immediately

## Image Workflow

If the app has a chart:

- use `forgekit get` against the chart definition to resolve image targets

If the app has no chart:

- resolve image targets from `version-control.yaml`

If the app has no image targets:

- exit immediately

## Binary Workflow

If the app is a binary app:

- build multi-platform binaries
- generate checksums
- upload GitHub Release assets

If the app is not a binary app:

- exit immediately

## Workflow Contract

Workflows should remain thin callers over the shared release logic.
