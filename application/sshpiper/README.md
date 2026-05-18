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
  allowedTargets:
    - gitea-ssh.basic-components
    - "*.codespace"
```

Notes:

- Allowlist is validation-only.
- Target is never rewritten. `alice.codespace` routes to upstream host `alice.codespace`.
- Built-in default Lua script is embedded in chart `ConfigMap`.
- Set `config.script` to a non-empty value if you want to fully override routing logic.

## Troubleshooting

- Invalid username format (must be `user+target` or `user+target+port`) will be rejected.
- Non-allowlisted target will be rejected.
- Invalid port in username will be rejected.
