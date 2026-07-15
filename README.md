# k8s at home

## introduction

This project is to develop and maintain k8s applications at home.

## application

* [aria2](application/aria2/README.md)
* [clash](application/clash/README.md)
* [yacd](application/yacd/README.md)
* [podman-in-container](application/podman-in-container/README.md)
* [codespace](application/codespace/README.md)
* [sub2api](application/sub2api/chart/README.md)

## contributing

We welcome contributions.

### Codex

Repository-level Codex customization is stored in:

- `AGENTS.md` for persistent repository guidance
- `.codex/config.toml` for shared project settings
- `.agents/skills` for repository skills

Provider endpoints and credentials belong in user-level Codex configuration,
not in the repository.

### version management

Applications use semver.

Version information is managed through:

- container `VERSION` files
- chart `Chart.yaml` `version` and `appVersion`
- `version-control.yaml` as the canonical app registry

Bootstrap a repo-local `forgekit` binary with:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version get
```

### query versions

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version get

FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version get <app-name>
```

Examples:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version get podman-in-container
"$FORGEKIT_BIN" version get agent-task-manager
```

### bump versions

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump <binary|container|chart> <app-name> <major|minor|patch>
```

Examples:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump chart podman-in-container patch
"$FORGEKIT_BIN" version bump container agent-task-manager-frontend patch
```

Sync linked chart values when bumping a chart:

```bash
FORGEKIT_BIN="$(bash ./setup/forgekit.sh)"
"$FORGEKIT_BIN" version bump chart aria2 minor --sync
```

### release model

Release is tag-driven and documented in:

- `.agents/skills/release-version/SKILL.md`

Use that skill as the source of truth for:

- push -> lint -> tag ordering
- release tag format
- workflow monitoring
- chart/container/binary version bump flow

The skill records the expected forgekit version in
`metadata.forgekit-version`. Review and update the skill whenever the default
version in `setup/forgekit.sh` changes.

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

With `forgekit 0.6.1`, app names must not contain `/`.

### pull request checklist

- [ ] Version numbers updated with `forgekit`
- [ ] Linked chart values synchronized when needed
- [ ] Release flow still matches `.agents/skills/release-version/SKILL.md`
