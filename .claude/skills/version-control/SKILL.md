---
name: version-control
description: Manage chart and image versions. Use for version queries, bumping versions.
---

# Version Control

Follows [Semantic Versioning 2.0](https://semver.org/)

## Core Files

| File | Purpose |
|------|---------|
| `application/<app>/container/VERSION` | Image version source of truth |
| `application/<app>/chart/Chart.yaml` | Chart version, appVersion, and `k8s-at-home.io/images` annotation |
| `application/<app>/chart/values.yaml` | Image tags (synced from VERSION files) |

## Scripts

| Script | Purpose |
|--------|---------|
| `tools/get-version.sh` | Query application versions |
| `tools/bump-image-version.sh` | Bump container image version |
| `tools/bump-chart-version.sh` | Bump chart version, optionally sync image tags |

### get-version.sh Usage

```bash
# List all versions
bash tools/get-version.sh

# Get chart version
bash tools/get-version.sh <app-name> chart

# Get image version
bash tools/get-version.sh <app-name> image

# Get chart appVersion
bash tools/get-version.sh <app-name> appVersion

# Multi-container apps
bash tools/get-version.sh aria2/aria2 image
bash tools/get-version.sh aria2/aria-ng image
```

### bump-image-version.sh Usage

```bash
# Bump image version
bash tools/bump-image-version.sh <app-path> <major|minor|patch>

# Non-interactive mode
echo y | bash tools/bump-image-version.sh <app-path> <major|minor|patch>

# Examples
bash tools/bump-image-version.sh podman-in-container patch
bash tools/bump-image-version.sh aria2/aria2 minor
```

### bump-chart-version.sh Usage

```bash
# Bump chart version only
bash tools/bump-chart-version.sh <chart-name> <major|minor|patch>

# Bump chart version and sync images
bash tools/bump-chart-version.sh <chart-name> <major|minor|patch> --sync-images

# Non-interactive mode
echo y | bash tools/bump-chart-version.sh <chart-name> <major|minor|patch> [--sync-images]

# Examples
bash tools/bump-chart-version.sh aria2 patch
bash tools/bump-chart-version.sh podman-in-container patch --sync-images
```

## Workflows

### Update Container Image

```bash
# 1. Modify Dockerfile/container code
# 2. Bump image version
echo y | bash tools/bump-image-version.sh <app-path> <patch|minor|major>
# 3. Bump chart version and sync
echo y | bash tools/bump-chart-version.sh <chart-name> <patch|minor|major> --sync-images
# 4. Test
bash tests/build-images.sh <image-name>
```

### Update Chart Only

```bash
# 1. Modify chart templates/values
# 2. Bump chart version (no sync)
echo y | bash tools/bump-chart-version.sh <chart-name> <patch|minor|major>
# 3. Test
bash tests/ct-lint.sh <chart-name>
```

### Multi-Container Application

```bash
# 1. Bump each image
echo y | bash tools/bump-image-version.sh aria2/aria2 minor
echo y | bash tools/bump-image-version.sh aria2/aria-ng patch
# 2. Sync to chart
echo y | bash tools/bump-chart-version.sh aria2 minor --sync-images
# 3. Test
bash tests/build-images.sh aria2 aria-ng
bash tests/ct-lint.sh aria2
```

## Version Bump Guidelines

- **major**: Breaking changes (API changes, removed features)
- **minor**: New features (backward compatible)
- **patch**: Bug fixes, optimizations, documentation

## Important

**Never bump versions without user confirmation.** When a version bump seems needed, ask the user first before executing any bump script.
