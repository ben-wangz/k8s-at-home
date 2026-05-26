# k8s at home

## introduction

This project is to develop and maintain k8s applications at home.

## application

* [aria2](application/aria2/README.md)
* [clash](application/clash/README.md)
* [yacd](application/yacd/README.md)
* [podman-in-container](application/podman-in-container/README.md)
* [sub2api](application/sub2api/chart/README.md)

## contributing

We welcome contributions. This section explains the versioning and release workflow used in this repository.

### project structure

```text
k8s-at-home/
├── application/
│   ├── <app-name>/
│   │   ├── chart/
│   │   ├── container/
│   │   └── cli/
├── build/
├── scripts/
├── setup/
├── version-control.yaml
└── tests/
```

### version management

Applications use semver.

Version information is managed through:

- container `VERSION` files
- chart `Chart.yaml` `version` and `appVersion`
- `version-control.yaml` as the canonical app registry

Bootstrap a repo-local `forgekit` binary with:

```bash
FORGEKIT_BIN=$(bash setup/forgekit.sh)
"${FORGEKIT_BIN}" version get
```

### query versions

```bash
# List all apps
forgekit version get

# Get one app
forgekit version get <app-name>
```

Examples:

```bash
forgekit version get podman-in-container
forgekit version get aria2-core
forgekit version get aria2-frontend
forgekit version get agent-task-manager
```

### bump versions

```bash
forgekit version bump <binary|container|chart> <app-name> <major|minor|patch>
```

Examples:

```bash
forgekit version bump chart podman-in-container patch
forgekit version bump container aria2-core minor
forgekit version bump container aria2-frontend major
```

Sync linked chart values when bumping a chart:

```bash
forgekit version bump chart aria2 minor --sync
```

### release model

Release is tag-driven.

- tag format: `<app-name>-v<semver>`
- chart release workflow: `.github/workflows/release-chart.yaml`
- container release workflow: `.github/workflows/release-container.yaml`
- binary release workflow: `.github/workflows/release-binary.yaml`
- validation workflow: `.github/workflows/lint.yaml`

Examples:

```bash
git tag aria2-v1.1.1
git tag agent-task-manager-v0.1.0
```

### chart metadata

Charts use annotations to define linked release targets.

```yaml
annotations:
  podman-in-container/images: |
    - name: podman-in-container-runtime
      path: application/podman-in-container/container
      valuesKey: image.tag
```

- `name`: globally unique app name used by `forgekit`
- `path`: target directory relative to repo root
- `valuesKey`: dot-notation key in `values.yaml`

With `forgekit 0.6.0`, app names must not contain `/`.

### testing

```bash
bash tests/build-images.sh [image-names...]
```

### pull request checklist

- [ ] Version numbers updated with `forgekit`
- [ ] Linked chart values synchronized when needed
- [ ] Images build successfully when container changes are made
- [ ] Release workflows still match the intended app/tag model
