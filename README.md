# k8s at home

## introduction

This project is to develop and maintain k8s applications at home.

## application

* [aria2](application/aria2/README.md)
* [clash](application/clash/README.md)
* [yacd](application/yacd/README.md)
* [podman-in-container](application/podman-in-container/README.md)

## contributing

We welcome contributions! This section explains the development workflow and tools available in this project.

### project structure

```
k8s-at-home/
├── application/            # Applications (charts and containers)
│   ├── <app-name>/
│   │   ├── chart/         # Helm chart
│   │   └── container/     # Container image source
│   │       └── VERSION    # Image version file (semver)
├── build/forgekit/         # Unified version/publish CLI source
├── version-control.yaml    # forgekit chart registry
└── tests/                  # Test scripts
```

### version management

All applications follow [Semantic Versioning](https://semver.org/) (semver). Version information is managed through:

- **Container images**: `application/<app>/container/**/VERSION`
- **Helm charts**: `application/<app>/chart/Chart.yaml` (`version` and `appVersion`)
- **Chart registry**: `version-control.yaml` (all managed chart paths)

### development tools

Version and publish operations are now unified under `forgekit`.

Install `forgekit` from GitHub Releases (recommended) or build from `build/forgekit/`.

You can bootstrap a repo-local binary with:

```bash
FORGEKIT_BIN=$(bash setup/forgekit.sh)
"${FORGEKIT_BIN}" version get
```

#### 1. Query versions

```bash
# List all chart/image versions
forgekit version get

# Get chart version
forgekit version get chart <chart-name>

# Get image/module version
forgekit version get <module>

# Get chart appVersion
forgekit version get chart appVersion <chart-name>
```

Examples:

```bash
forgekit version get chart podman-in-container
forgekit version get aria2/aria2
forgekit version get aria2/aria-ng
```

#### 2. Bump image version

```bash
forgekit version bump <module> <major|minor|patch>
```

Examples:

```bash
forgekit version bump podman-in-container patch
forgekit version bump aria2/aria2 minor
forgekit version bump aria2/aria-ng major
```

#### 3. Bump chart version

```bash
# Chart only
forgekit version bump-chart <chart-name> <major|minor|patch>

# Chart + sync image versions to appVersion and values.yaml
forgekit version bump-chart <chart-name> <major|minor|patch> --sync
```

### typical workflows

#### Workflow 1: Update container image

```bash
# 1. Make changes to container code/Containerfile
vim application/podman-in-container/container/Containerfile

# 2. Bump image version
forgekit version bump podman-in-container patch

# 3. Bump chart version and sync images
forgekit version bump-chart podman-in-container patch --sync

# 4. Test
bash tests/build-images.sh podman-in-container

# 5. Review and commit
git diff
git add application/podman-in-container/
git commit -m "feat(podman-in-container): optimize Containerfile layers"
```

#### Workflow 2: Update chart only

```bash
# 1. Make chart changes
vim application/aria2/chart/templates/deployment.yaml

# 2. Bump chart version
forgekit version bump-chart aria2 patch

# 3. Test chart
bash tests/ct-lint.sh aria2
```

#### Workflow 3: Update multi-container application

```bash
# 1. Bump backend image
forgekit version bump aria2/aria2 minor

# 2. Bump frontend image
forgekit version bump aria2/aria-ng patch

# 3. Sync to chart and bump chart version
forgekit version bump-chart aria2 minor --sync

# 4. Test
bash tests/build-images.sh aria2 aria-ng
bash tests/ct-lint.sh aria2
```

### version synchronization metadata

Charts use annotations to define image metadata for automatic synchronization:

```yaml
# application/<app>/chart/Chart.yaml
annotations:
  podman-in-container/images: |
    - name: podman-in-container
      path: application/podman-in-container/container
      valuesKey: image.tag
```

- **name**: module name used by `forgekit version get/bump`
- **path**: path to the directory that contains `VERSION` (relative to repo root)
- **valuesKey**: dot-notation key in `values.yaml`

### testing

```bash
# Test image builds
bash tests/build-images.sh [image-names...]

# Test chart linting
bash tests/ct-lint.sh [chart-name]
```

### pull request checklist

- [ ] Version numbers updated with `forgekit`
- [ ] Image/chart versions are synchronized when needed
- [ ] Images build successfully (`bash tests/build-images.sh`)
- [ ] Charts pass linting (`bash tests/ct-lint.sh`)
