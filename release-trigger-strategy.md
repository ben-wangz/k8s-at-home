# Release Trigger Strategy

## Goal

Define the tag-driven logic shared by release workflows.

## Shared Model

1. A tag has the form `<app-name>-v<semver>`.
2. The tag identifies one app release event.
3. Each workflow is independent and only acts on the artifact type it owns.
4. Workflows that do not apply to the tagged app exit immediately.
5. `forgekit version get <app-name> --output json` is the machine-readable source of truth for app type, version, path, and linked targets.

## Shared Cascade Logic

The shared release logic should:

- parse the tag and resolve `app-name`
- call `forgekit version get <app-name> --output json`
- read the resolved app type
- for chart apps, read linked targets returned from chart annotations
- decide whether the app has a chart release target
- decide whether the app has container release targets
- decide whether the app has binary release targets
- return no-op when the app does not own that artifact type

## Chart Workflow

If the app has a chart:

- publish the chart artifact

If the app has no chart:

- exit immediately

## Container Workflow

If the app has a chart:

- use linked targets from `forgekit version get <app-name> --output json`
- publish only linked targets with `type: container`

If the app is a standalone container app:

- publish that container directly

If the app has no container targets:

- exit immediately

## Binary Workflow

If the app is a standalone binary app:

- build multi-platform binaries
- generate checksums
- upload GitHub Release assets

If the app has a chart-linked binary target set in the future:

- use linked targets from `forgekit version get <app-name> --output json`
- build only linked targets with `type: binary`

If the app has no binary targets:

- exit immediately

## Workflow Contract

Workflows should remain thin callers over tag parsing, `forgekit version get`, and artifact-specific publish steps.
