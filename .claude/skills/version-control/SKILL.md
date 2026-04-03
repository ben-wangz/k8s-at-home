---
name: version-control
description: Manage chart and image versions through forgekit.
---

# Version Control

Follows [Semantic Versioning 2.0](https://semver.org/)

## Instructions

When this skill is invoked, execute the following steps:

1. **Parse arguments** to determine:
   - Action: `bump`, `get`, `query`, `sync`
   - Target: `image`, `chart`, or both
   - Bump type: `major`, `minor`, `patch`
   - App/module name: from args, git status, or context

2. **Determine app name** if not explicitly provided:
   - Check git status for modified files in `application/*/container/` or `application/*/chart/`
   - Extract app/module from the path
   - If multiple apps/modules or unclear, ask user

3. **Ensure forgekit binary is ready**:
   - `FORGEKIT_BIN="$(bash setup/forgekit.sh)"`
   - Always use `${FORGEKIT_BIN}` for version operations
   - Keep min/best version defaults only in `setup/forgekit.sh`

4. **Execute forgekit command**:
   - **Bump image version**: `${FORGEKIT_BIN} version bump <module> <bump-type>`
   - **Bump chart version**: `${FORGEKIT_BIN} version bump-chart <chart-name> <bump-type>`
   - **Bump chart with sync**: `${FORGEKIT_BIN} version bump-chart <chart-name> <bump-type> --sync`
   - **Get all versions**: `${FORGEKIT_BIN} version get`
   - **Get image/module version**: `${FORGEKIT_BIN} version get <module>`
   - **Get chart version**: `${FORGEKIT_BIN} version get chart <chart-name>`
   - **Get chart appVersion**: `${FORGEKIT_BIN} version get chart appVersion <chart-name>`
   - **Sync image versions**: `${FORGEKIT_BIN} version sync [chart-name]`

5. **Report results** to user with version changes

### Argument Parsing Examples

- `bump patch image` -> Bump image patch version for module from context
- `bump minor chart podman-in-container` -> Bump chart minor version for podman-in-container
- `bump patch` -> Bump both image and chart patch version
- `get version` -> List all versions
- `get image aria2/aria2` -> Get aria2/aria2 module version

## Core Files

| File | Purpose |
|------|---------|
| `setup/forgekit.sh` | Ensure/install forgekit with repo-defined version policy |
| `build/bin/forgekit` | Repo-local forgekit binary managed by setup script |
| `version-control.yaml` | forgekit chart registry |
| `application/<app>/container/**/VERSION` | Image version source of truth |
| `application/<app>/chart/Chart.yaml` | Chart version, appVersion, and `<chart-name>/images` annotation |
| `application/<app>/chart/values.yaml` | Image tags (synced from VERSION files) |

## Commands

### List/Get Version

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"

# List all versions
"${FORGEKIT_BIN}" version get

# Get chart version
"${FORGEKIT_BIN}" version get chart <chart-name>

# Get image/module version
"${FORGEKIT_BIN}" version get <module>

# Get chart appVersion
"${FORGEKIT_BIN}" version get chart appVersion <chart-name>

# Multi-container modules
"${FORGEKIT_BIN}" version get aria2/aria2
"${FORGEKIT_BIN}" version get aria2/aria-ng
```

### Bump Image Version

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"
"${FORGEKIT_BIN}" version bump <module> <major|minor|patch>

# Examples
"${FORGEKIT_BIN}" version bump podman-in-container patch
"${FORGEKIT_BIN}" version bump aria2/aria2 minor
```

### Bump Chart Version

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"

# Bump chart version only
"${FORGEKIT_BIN}" version bump-chart <chart-name> <major|minor|patch>

# Bump chart version and sync images
"${FORGEKIT_BIN}" version bump-chart <chart-name> <major|minor|patch> --sync

# Examples
"${FORGEKIT_BIN}" version bump-chart aria2 patch
"${FORGEKIT_BIN}" version bump-chart podman-in-container patch --sync
```

## Workflows

### Update Container Image

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"

# 1. Modify Dockerfile/container code
# 2. Bump image version
"${FORGEKIT_BIN}" version bump <module> <patch|minor|major>
# 3. Bump chart version and sync
"${FORGEKIT_BIN}" version bump-chart <chart-name> <patch|minor|major> --sync
# 4. Test
bash tests/build-images.sh <image-name>
```

### Update Chart Only

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"

# 1. Modify chart templates/values
# 2. Bump chart version (no sync)
"${FORGEKIT_BIN}" version bump-chart <chart-name> <patch|minor|major>
# 3. Test
bash tests/ct-lint.sh <chart-name>
```

### Multi-Container Application

```bash
FORGEKIT_BIN="$(bash setup/forgekit.sh)"

# 1. Bump each image
"${FORGEKIT_BIN}" version bump aria2/aria2 minor
"${FORGEKIT_BIN}" version bump aria2/aria-ng patch
# 2. Sync to chart
"${FORGEKIT_BIN}" version bump-chart aria2 minor --sync
# 3. Test
bash tests/build-images.sh aria2 aria-ng
bash tests/ct-lint.sh aria2
```

## Version Bump Guidelines

- **major**: Breaking changes (API changes, removed features)
- **minor**: New features (backward compatible)
- **patch**: Bug fixes, optimizations, documentation

## Important

**Never bump versions without user confirmation.** When a version bump seems needed, ask the user first before executing any `forgekit version bump*` command.
