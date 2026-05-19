# sshpiper

`sshpiper` provides an SSH reverse proxy entry in Kubernetes.

## Quick start

1. Get chart version:

```bash
export CHART_VERSION=$(forgekit --project-root ../.. version get chart sshpiper)
```

2. Install:

```bash
helm upgrade --install sshpiper oci://ghcr.io/ben-wangz/k8s-at-home-charts/sshpiper \
  --atomic \
  --version ${CHART_VERSION} \
  --namespace basic-components \
  --create-namespace
```

## Route format

- `<upstream-user>+<target>@sshpiper.local` (default target port `22`)
- `<upstream-user>+<target>+<target-port>@sshpiper.local`

Examples:

```bash
ssh git+gitea-ssh.basic-components@<node-ip> -p 32022
ssh git+gitea-ssh.basic-components+22@<node-ip> -p 32022
ssh root+alice.codespace@<node-ip> -p 32022
```

## Allowlist example

```yaml
config:
  authMode: key
  hostKeyPolicy: tofu
  allowedTargets:
    - gitea-ssh.basic-components
    - "*.codespace"
```

Notes:

- Allowlist is validation-only.
- Target is never rewritten. `alice.codespace` routes to upstream host `alice.codespace`.
- `config.authMode` supports `password` and `key`.
- Default mode is `key`.
- `password` mode is password-only on both downstream and upstream paths.
- `key` mode disables password auth and requires mounted key material for the custom plugin.
- `hostKeyPolicy` defaults to `tofu`.
- Listener uses env vars (`SSHPIPERD_ADDRESS` and `SSHPIPERD_PORT`) from `listen.address` and `listen.port`.

## Known hosts storage

Default behavior uses `emptyDir` for writable known hosts state:

```yaml
storage:
  knownHosts:
    type: emptyDir
    existingClaim: ""
```

To persist TOFU state across pod recreation, use a PVC:

```yaml
storage:
  knownHosts:
    type: pvc
    existingClaim: sshpiper-known-hosts
```

For `strict` mode, you can also fully manage host key files yourself with `extraVolumes` and `extraVolumeMounts`.

## Implementation note

- The chart is now aligned to a custom Go plugin path.
- The Go plugin source should live in `application/sshpiper/src/`.
- The container files should live in `application/sshpiper/container/`.
- The image build context should be `application/sshpiper/`.
- The runtime image should be rebuilt from the upstream official image and include the custom plugin binary at `config.pluginPath`.
- Default auth material paths exposed to the plugin are `authFiles.authorizedKeysPath` and `authFiles.upstreamPrivateKeyPath`.
- In `key` mode, provide `authFiles.existingSecret` or mount equivalent files with `extraVolumes` and `extraVolumeMounts`.

## Troubleshooting

- Invalid username format (must be `user+target` or `user+target+port`) will be rejected.
- Non-allowlisted target will be rejected.
- Invalid port in username will be rejected.
