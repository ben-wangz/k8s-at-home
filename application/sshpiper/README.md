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
auth:
  mode: key
  hostKeyPolicy: tofu
  allowedTargets:
    - gitea-ssh.basic-components
    - "*.codespace"
```

Notes:

- Allowlist is validation-only.
- Target is never rewritten. `alice.codespace` routes to upstream host `alice.codespace`.
- `auth.mode` supports `password` and `key`.
- Default mode is `key`.
- `password` mode is password-only on both downstream and upstream paths.
- `key` mode disables password auth and requires mounted key material for the custom plugin.
- `auth.hostKeyPolicy` defaults to `tofu`.
- Listener uses env vars (`SSHPIPERD_ADDRESS` and `SSHPIPERD_PORT`) from `listen.address` and `listen.port`.
- The default generated server host key is `ed25519` and is stored under `ssh_host_ed25519_key`.

## Key mode auth materials

Downstream client keys are maintained as an array in values:

```yaml
auth:
  authorizedKeys:
    values:
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... user1
      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAJ... user2
```

The chart renders these entries into a dedicated `authorized_keys` Secret.

Upstream SSH login uses a separate keypair Secret.

If you do not provide one, the chart automatically generates an `ed25519` upstream keypair Secret.

You can also provide an existing Secret:

```yaml
auth:
  upstreamKeypair:
    existingSecret: sshpiper-upstream-keypair
```

The upstream Secret only needs to contain `id_ed25519`. The plugin uses only the private key.

## Known hosts storage

Default behavior uses `emptyDir` for writable known hosts state:

```yaml
storage:
  knownHosts:
    enabled: false
    existingClaim: ""
```

To persist TOFU state across pod recreation, enable PVC-backed storage. By default the chart creates the PVC for you:

```yaml
storage:
  knownHosts:
    enabled: true
    size: 1Gi
```

To reuse an existing PVC instead:

```yaml
storage:
  knownHosts:
    enabled: true
    existingClaim: sshpiper-known-hosts
```

For `strict` mode, you can also fully manage host key files yourself with `extraVolumes` and `extraVolumeMounts`.

## Implementation note

- The chart is now aligned to a custom Go plugin path.
- The Go plugin source should live in `application/sshpiper/src/`.
- The container files should live in `application/sshpiper/container/`.
- The image build context should be `application/sshpiper/`.
- The runtime image is rebuilt from the upstream official image and starts the bundled custom plugin through an internal `entrypoint.sh`.
- Default auth material paths exposed to the plugin are `auth.authorizedKeys.filePath` and `auth.upstreamKeypair.privateKeyPath`.
- In `key` mode, downstream `authorized_keys` and upstream keypair are managed as two separate Secrets.

## Troubleshooting

- Invalid username format (must be `user+target` or `user+target+port`) will be rejected.
- Non-allowlisted target will be rejected.
- Invalid port in username will be rejected.

## Acknowledgements

Thanks to GitHub user [`qipan2333`](https://github.com/qipan2333) for the initial technical feasibility validation of this approach.
