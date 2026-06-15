# Tabminal Helm Chart Design

## Goal

Add `application/tabminal/chart` as a Helm chart for the upstream Tabminal application.

Tabminal already publishes an official container image at `docker.io/leask/tabminal`, so this project should only package Kubernetes deployment manifests and should not build or maintain a custom container image.

## Background

Tabminal is a browser-based terminal and ACP agent workspace. It exposes a Koa HTTP server and WebSocket endpoints on port `9846` by default.

Important upstream behavior from `/root/temp/Tabminal`:

- Official image: `docker.io/leask/tabminal`.
- Default container entrypoint: `tabminal`.
- Default command in the image is `--help`, so the chart must override args or environment to start the server.
- Required startup acknowledgement: `--accept-terms`, `TABMINAL_ACCEPT=true`, or `TABMINAL_ACCEPT_TERMS=true`.
- Default bind address is `127.0.0.1`; Kubernetes needs `TABMINAL_HOST=0.0.0.0`.
- Default port is `9846` and can be controlled with `TABMINAL_PORT`.
- Health endpoint: `GET /healthz`.
- Runtime state is stored under `~/.tabminal/`, including session snapshots, cluster state, agent tab state, agent config, auth sessions, and optional config.
- If no password is configured, Tabminal generates a temporary password at startup. For Kubernetes, the chart should support an explicit password secret.

## Repository Fit

Follow the existing application layout:

```text
application/tabminal/
  README.md
  chart/
    Chart.yaml
    values.yaml
    templates/
      _helpers.tpl
      deployment.yaml
      service.yaml
      ingress.yaml
      pvc.yaml
      secret.yaml
      extra-list.yaml
```

Use the same conventions as the existing single-container charts such as `ddns-go` and `yacd`:

- Use Bitnami `common` chart dependency for names, labels, capabilities, affinity presets, image rendering, and resource presets.
- Expose `commonLabels`, `commonAnnotations`, `podLabels`, `podAnnotations`, `imagePullSecrets`, affinity, node selector, tolerations, and `extraDeploy` values.
- Keep chart templates small and explicit instead of introducing a large abstraction layer.

## Chart Metadata

Proposed `Chart.yaml` fields:

- `apiVersion: v2`
- `name: tabminal`
- `description: A cloud-native browser terminal and ACP agent workspace`
- `type: application`
- `home: https://github.com/leask/tabminal`
- `sources:`
  - `https://github.com/leask/tabminal`
  - `https://github.com/ben-wangz/k8s-at-home/tree/main/application/tabminal`
- `keywords:`
  - `terminal`
  - `web-terminal`
  - `workspace`
  - `acp`
- Dependency on `common` from `oci://registry-1.docker.io/bitnamicharts`, version `2.x.x`.

Version values must be changed through the repository version workflow only. Do not manually edit chart version or image tag during release work.

## Values Design

Default `values.yaml` should start minimal and safe:

```yaml
replicas: 1

image:
  repository: docker.io/leask/tabminal
  tag: latest
  pullPolicy: IfNotPresent

ports:
  http: 9846

tabminal:
  host: 0.0.0.0
  acceptTerms: false
  password: ""
  existingSecret: ""
  existingSecretPasswordKey: password
  shell: ""
  openrouterKey: ""
  openaiKey: ""
  openaiApi: ""
  model: ""
  googleKey: ""
  googleCx: ""
  cloudflareKey: ""
  heartbeat: ""
  history: ""
  debug: false
  extraEnvVars: {}
  extraEnvVarsSecret: ""

persistence:
  enabled: true
  mountPath: /root/.tabminal
  storageClass: ""
  existingClaim: ""
  accessModes:
    - ReadWriteOnce
  size: 1Gi
  subPath: ""
  annotations: {}
  selector: {}
  dataSource: {}

service:
  type: ClusterIP
  ports:
    http: 9846
    httpNodePort: ""
  annotations: {}
  clusterIP: ""
  sessionAffinity: ""
  sessionAffinityConfig: {}
  externalTrafficPolicy: ""
  loadBalancerSourceRanges: []
  loadBalancerIP: ""
  loadBalancerClass: ""
  extraPorts: []

ingress:
  enabled: false
  annotations: {}
  ingressClassName: nginx
  hostname: tabminal.example.com
  path: /
  pathType: Prefix
  tls: false
```

Notes:

- `tabminal.acceptTerms` should default to `false` so users explicitly opt in to upstream's security warning. If false, the pod will not start, which is intentional and visible.
- `tabminal.password` is convenient for simple installs but should be rendered into a Secret, not directly into the Deployment.
- `tabminal.existingSecret` should be preferred for production.
- AI provider keys and Cloudflare/Google credentials should also be Secret-backed. For simplicity, initially support rendering them into the chart-managed Secret when values are provided, plus `extraEnvVarsSecret` for externally managed secret refs.
- `openrouterKey` and `openaiKey` are mutually exclusive upstream. Document this; optionally enforce it with a Helm template `fail`.

## Kubernetes Resources

### Deployment

Run a single container named `tabminal`.

Required environment:

- `TABMINAL_HOST`: from `tabminal.host`, default `0.0.0.0`.
- `TABMINAL_PORT`: from `ports.http`.
- `TABMINAL_ACCEPT_TERMS`: from `tabminal.acceptTerms`.

Optional environment:

- `TABMINAL_PASSWORD`: from Secret key.
- `TABMINAL_SHELL`
- `TABMINAL_OPENROUTER_KEY`
- `TABMINAL_OPENAI_KEY`
- `TABMINAL_OPENAI_API`
- `TABMINAL_MODEL`
- `TABMINAL_GOOGLE_KEY`
- `TABMINAL_GOOGLE_CX`
- `TABMINAL_CLOUDFLARE_KEY`
- `TABMINAL_HEARTBEAT`
- `TABMINAL_HISTORY`
- `TABMINAL_DEBUG`

Container args should start the server rather than showing help. Two viable options:

- `args: ["--accept-terms"]` when `tabminal.acceptTerms` is true.
- Prefer environment-only startup with command args omitted if upstream starts when no args are passed. Because the upstream image default `CMD` is `--help`, the chart should explicitly set `args` to `[]` or `--accept-terms` after verifying rendered Pod behavior.

Recommended implementation:

- Set `args` to `[]` plus environment variables if Kubernetes preserves an explicit empty args list as intended.
- If that is not reliable, set `args: ["--accept-terms"]` when `tabminal.acceptTerms=true` and fail rendering otherwise.

Probes:

- `startupProbe`: HTTP `GET /healthz` on `ports.http`.
- `livenessProbe`: HTTP `GET /healthz` on `ports.http`.
- `readinessProbe`: HTTP `GET /healthz` on `ports.http`.
- Support `customStartupProbe`, `customLivenessProbe`, and `customReadinessProbe` like existing charts.

Persistence:

- Mount the PVC at `persistence.mountPath`, default `/root/.tabminal`, matching the official image's likely root home directory.
- Keep `replicas: 1` by default. Multiple replicas would not share live terminal sessions safely and would require sticky sessions plus shared or per-replica state decisions.

Security context:

- Start with the same chart knobs used elsewhere: `podSecurityContext.enabled`, `fsGroup`, `fsGroupChangePolicy`, and supplemental groups.
- Avoid a strict non-root container security context initially unless verified against the official image, because Tabminal manages PTYs, shells, and home-directory state.

### Service

Expose one TCP port:

- name: `http`
- service port: `service.ports.http`
- target port: `ports.http`

Support `ClusterIP`, `NodePort`, and `LoadBalancer` options using the same pattern as `yacd` and `ddns-go`.

### Ingress

Expose HTTP and WebSocket traffic through the same backend service.

Important notes:

- Ingress controller must support WebSocket upgrades.
- Do not recommend public unauthenticated internet exposure.
- Prefer VPN, Tailscale, Cloudflare Access, or another Zero Trust layer in front of the ingress.

Use the same basic ingress values shape as existing charts. Keep `ingress.enabled=false` by default.

### PVC

Create a PVC only when `persistence.enabled=true` and `persistence.existingClaim` is empty.

The PVC stores `~/.tabminal`, including auth/session/agent state. This data can contain sensitive terminal and host metadata, so it should be treated as sensitive application state.

### Secret

Create a Secret when at least one chart-managed sensitive value is set and `tabminal.existingSecret` is empty.

Suggested keys:

- `password`
- `openrouter-key`
- `openai-key`
- `google-key`
- `google-cx`
- `cloudflare-key`

The Deployment should read each configured value via `valueFrom.secretKeyRef`.

### Extra Deploy

Include `templates/extra-list.yaml` so users can add NetworkPolicy, middleware, additional Secrets, or controller-specific resources without forking the chart.

## Security Considerations

Tabminal is high privilege by design: it provides browser access to a real shell and host-local files inside the container.

Chart documentation must make these constraints explicit:

- Require `tabminal.acceptTerms=true` before the application starts.
- Require a stable password for normal deployments; do not rely on the generated temporary password in Kubernetes.
- Prefer `existingSecret` for credentials.
- Keep ingress disabled by default.
- Recommend an external access-control layer for any remote access.
- Treat the PVC as sensitive because it can contain session snapshots, auth state, cluster host registry, agent settings, and command context.
- Warn that AI keys may send terminal context, file paths, or command history to the configured provider, consistent with upstream behavior.

## Initial Implementation Plan

1. Create `application/tabminal/chart` by adapting the `yacd` chart structure.
2. Add `Chart.yaml` with the Bitnami `common` dependency and upstream Tabminal metadata.
3. Add `values.yaml` with image, port, Tabminal config, service, ingress, persistence, probes, resources, and extra deploy values.
4. Add `secret.yaml` for chart-managed credentials and support `existingSecret` for production use.
5. Add `deployment.yaml` with required environment, optional secret-backed environment, PVC mount, probes, resources, scheduling knobs, and image pull secrets.
6. Add `service.yaml`, `ingress.yaml`, `pvc.yaml`, `_helpers.tpl`, and `extra-list.yaml` following existing templates.
7. Add `application/tabminal/README.md` with install examples and security warnings.
8. Render the chart with representative values and verify the generated manifests.
9. Run a smoke install in a disposable namespace and verify `GET /healthz` and WebSocket UI access through port-forward.

## Example Values

Minimal local/cluster-internal install:

```yaml
tabminal:
  acceptTerms: true
  password: "change-me"

ingress:
  enabled: false
```

Production-style install using an existing Secret:

```yaml
tabminal:
  acceptTerms: true
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password

persistence:
  enabled: true
  size: 5Gi

ingress:
  enabled: true
  hostname: tabminal.example.com
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
```

Example Secret:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: tabminal-secret
type: Opaque
stringData:
  password: replace-with-a-strong-password
  openrouter-key: optional-openrouter-key
```

## Open Questions

- Confirm whether the official image runs correctly with an explicit empty args list, or whether the chart should always use `args: ["--accept-terms"]` when accepted.
- Confirm the official image runtime user. If it is not root, adjust the default `persistence.mountPath` from `/root/.tabminal` to that user's home directory.
- Decide whether to enforce `replicas=1` with a template failure or only document it as the safe default.
- Decide whether the first chart version should pin the current upstream `3.0.40` image tag or use `latest`. Pinning is more reproducible, but version updates must follow this repository's version workflow.
