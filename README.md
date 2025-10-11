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
├── application/          # Applications (charts and containers)
│   ├── <app-name>/
│   │   ├── chart/       # Helm chart
│   │   └── container/   # Container image source
│   │       └── VERSION  # Image version file (semver)
├── tools/               # Version management and utility scripts
│   ├── lib/            # Shared library functions
│   ├── get-version.sh  # Query version information
│   ├── bump-image-version.sh     # Bump container image versions
│   └── bump-chart-version.sh     # Bump Helm chart versions
└── tests/              # Test scripts
```

### version management

All applications follow [Semantic Versioning](https://semver.org/) (semver). Version information is managed through:

- **Container images**: `application/<app>/container/VERSION` file
- **Helm charts**: `application/<app>/chart/Chart.yaml` (version and appVersion fields)

### development tools

#### 1. get-version.sh - Query versions

Get version information for applications.

**Usage:**
```bash
# List all versions
bash tools/get-version.sh

# Get chart version
bash tools/get-version.sh <app-name> chart

# Get image version
bash tools/get-version.sh <app-name> image

# Get chart appVersion
bash tools/get-version.sh <app-name> appVersion
```

**Examples:**
```bash
# List all chart and image versions
bash tools/get-version.sh

# Get podman-in-container chart version
bash tools/get-version.sh podman-in-container chart

# Get aria2 image version (multi-container app)
bash tools/get-version.sh aria2/aria2 image
bash tools/get-version.sh aria2/aria-ng image
```

#### 2. bump-image-version.sh - Bump image versions

Update container image versions following semver.

**Usage:**
```bash
bash tools/bump-image-version.sh <app-path> <major|minor|patch>

# Non-interactive mode (auto-confirm)
echo y | bash tools/bump-image-version.sh <app-path> <major|minor|patch>
```

**When to use:**
- **major**: Breaking changes (e.g., API changes, removed features)
- **minor**: New features (backward compatible)
- **patch**: Bug fixes, optimizations, documentation updates

**Examples:**
```bash
# Bump podman-in-container patch version (1.2.0 -> 1.2.1)
bash tools/bump-image-version.sh podman-in-container patch

# Bump aria2 minor version (1.0.0 -> 1.1.0)
bash tools/bump-image-version.sh aria2/aria2 minor

# Bump aria-ng major version (1.0.0 -> 2.0.0)
bash tools/bump-image-version.sh aria2/aria-ng major
```

**What it does:**
1. Reads current version from `VERSION` file
2. Calculates new version based on bump type
3. Prompts for confirmation
4. Updates `VERSION` file

#### 3. bump-chart-version.sh - Bump chart versions

Update Helm chart versions with optional image synchronization.

**Usage:**
```bash
bash tools/bump-chart-version.sh <chart-name> <major|minor|patch> [--sync-images]

# Non-interactive mode (auto-confirm)
echo y | bash tools/bump-chart-version.sh <chart-name> <major|minor|patch> [--sync-images]
```

**Options:**
- `--sync-images`: Synchronize image versions from VERSION files to Chart.yaml appVersion and values.yaml image tags

**When to use:**
- **Without --sync-images**: Chart-only changes (configuration, templates, documentation)
- **With --sync-images**: After bumping image versions, to synchronize chart with new images

**Examples:**
```bash
# Bump chart version only (chart changes without image updates)
bash tools/bump-chart-version.sh aria2 patch

# Bump chart version and sync image versions (after image updates)
bash tools/bump-chart-version.sh podman-in-container patch --sync-images
```

**What it does:**
1. Reads current chart version from `Chart.yaml`
2. Calculates new chart version
3. If `--sync-images` is specified:
   - Reads image versions from VERSION files (using `k8s-at-home.io/images` annotation)
   - Updates Chart.yaml `appVersion` to first image version
   - Updates values.yaml image tags
4. Prompts for confirmation
5. Updates all files

### typical workflows

#### Workflow 1: Update container image only

When you modify a Dockerfile or container code:

```bash
# 1. Make changes to container code/Dockerfile
vim application/podman-in-container/container/Dockerfile

# 2. Bump image version
echo y | bash tools/bump-image-version.sh podman-in-container patch

# 3. Bump chart version and sync images
echo y | bash tools/bump-chart-version.sh podman-in-container patch --sync-images

# 4. Test the build
bash tests/build-images.sh podman-in-container

# 5. Review and commit
git diff
git add application/podman-in-container/
git commit -m "feat(podman-in-container): optimize Dockerfile layers"
```

#### Workflow 2: Update chart only

When you modify chart templates, values, or configuration:

```bash
# 1. Make changes to chart
vim application/aria2/chart/templates/deployment.yaml

# 2. Bump chart version (no image sync needed)
echo y | bash tools/bump-chart-version.sh aria2 patch

# 3. Test the chart
bash tests/ct-lint.sh aria2

# 4. Review and commit
git diff
git add application/aria2/chart/
git commit -m "fix(aria2): correct resource limits in deployment"
```

#### Workflow 3: Update multi-container application

For applications with multiple containers (e.g., aria2):

```bash
# 1. Update aria2 backend image
echo y | bash tools/bump-image-version.sh aria2/aria2 minor

# 2. Update aria-ng frontend image
echo y | bash tools/bump-image-version.sh aria2/aria-ng patch

# 3. Sync all images to chart
echo y | bash tools/bump-chart-version.sh aria2 minor --sync-images

# 4. Test
bash tests/build-images.sh aria2 aria-ng
bash tests/ct-lint.sh aria2

# 5. Commit
git add application/aria2/
git commit -m "feat(aria2): update both aria2 and aria-ng versions"
```

### version synchronization metadata

Charts use annotations to define image metadata for automatic synchronization:

```yaml
# application/<app>/chart/Chart.yaml
annotations:
  k8s-at-home.io/images: |
    - name: podman-in-container
      path: podman-in-container
      valuesKey: image.tag
```

- **name**: Display name
- **path**: Path to VERSION file (relative to application/)
- **valuesKey**: Dot-notation path in values.yaml

### testing

```bash
# Test image builds
bash tests/build-images.sh [image-names...]

# Test chart linting
bash tests/ct-lint.sh [chart-name]

# Test chart linting with proxy (if dependency download fails)
http_proxy=http://proxy.example.com:8080 \
https_proxy=http://proxy.example.com:8080 \
no_proxy=localhost,127.0.0.1 \
  bash tests/ct-lint.sh [chart-name]

# Test version utilities
bash tests/bump-version.sh
bash tests/get-version.sh
```

### pull request checklist

When submitting a pull request:

- [ ] Version numbers updated using tools scripts
- [ ] All versions synchronized (image VERSION, Chart.yaml, values.yaml)
- [ ] Images build successfully (`bash tests/build-images.sh`)
- [ ] Charts pass linting (`bash tests/ct-lint.sh`)
- [ ] Changes documented in commit message
- [ ] Breaking changes noted in PR description
