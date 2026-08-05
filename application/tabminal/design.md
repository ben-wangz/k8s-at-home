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
      ssh-keygen.yaml
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
  tag: sha-65e7af9
  pullPolicy: IfNotPresent

ports:
  http: 9846

tabminal:
  host: 0.0.0.0
  acceptTerms: false
  existingSecret: ""
  existingSecretPasswordKey: password
  existingSecretOpenrouterKey: ""
  existingSecretOpenaiKey: ""
  existingSecretGoogleKey: ""
  existingSecretGoogleCx: ""
  existingSecretCloudflareKey: ""
  shell: ""
  openaiApi: ""
  model: ""
  heartbeat: ""
  history: ""
  debug: false
  extraEnvVars: []
  extraEnvVarsSecret: ""

ssh:
  enabled: false
  existingSecret: ""
  privateKeyKey: ssh-privatekey
  privateKeyFilename: id_ed25519
  publicKeyKey: ""
  knownHostsKey: ""
  configKey: ""
  keyGeneration:
    enabled: false
    outputSecretName: ""
    passphraseSecret:
      name: ""
      key: password
    serviceAccountTokenExpirationSeconds: 600
    activeDeadlineSeconds: 600
    backoffLimit: 0
    resources: {}

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
- `tabminal.existingSecret` is required. Credentials must not be supplied through Helm values.
- Do not generate a random password in a template. Random output changes on each Argo CD render and causes perpetual drift; `lookup` is not reliable in an offline GitOps renderer.
- Optional AI provider and Cloudflare/Google credentials are selected by their key names in `existingSecret`.
- `existingSecretOpenrouterKey` and `existingSecretOpenaiKey` are mutually exclusive upstream.
- `extraEnvVars` is a list of Kubernetes `EnvVar` objects. `extraEnvVarsSecret` supports an additional externally managed Secret.
- The official stable application version is `3.0.40`. Docker Hub does not publish a semantic-version tag, so the chart uses the corresponding official `sha-65e7af9` tag instead of mutable `latest`.
- `ssh.existingSecret` is required when `ssh.enabled=true` unless `ssh.keyGeneration.enabled=true`. SSH private key material and passphrases must not be supplied through Helm values.
- `ssh.existingSecret` and `ssh.keyGeneration.enabled` are mutually exclusive.
- The key generation passphrase comes only from `ssh.keyGeneration.passphraseSecret` and is not injected into the Tabminal container.

## Kubernetes Resources

### Deployment

Run a single container named `tabminal`.

Required environment:

- `TABMINAL_HOST`: from `tabminal.host`, default `0.0.0.0`.
- `TABMINAL_PORT`: from `ports.http`.
- `TABMINAL_ACCEPT_TERMS`: from `tabminal.acceptTerms`.
- `TABMINAL_PASSWORD`: from `tabminal.existingSecret` and `tabminal.existingSecretPasswordKey`.

Optional environment:

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

Container args must override the image's default `--help` command. Pass the configured host, port, password environment reference, optional settings, and `--accept-terms` when accepted.

Probes:

- `startupProbe`: HTTP `GET /healthz` on `ports.http`.
- `livenessProbe`: HTTP `GET /healthz` on `ports.http`.
- `readinessProbe`: HTTP `GET /healthz` on `ports.http`.
- Support `customStartupProbe`, `customLivenessProbe`, and `customReadinessProbe` like existing charts.

Persistence:

- Mount the PVC at `persistence.mountPath`, default `/root/.tabminal`, matching the official image's likely root home directory.
- Keep `replicas: 1` by default. Multiple replicas would not share live terminal sessions safely and would require sticky sessions plus shared or per-replica state decisions.

SSH client files:

- Mount the selected existing or generated SSH Secret read-only in an init container.
- Copy the selected private key, optional public key, `known_hosts`, and `config` files into a memory-backed `emptyDir`.
- Set the target directory to mode `0700` and files to `0600`, then mount it at `/root/.ssh` in the Tabminal container.
- Set public key files to mode `0644`.
- Keep the target writable so interactive SSH sessions can update `known_hosts`.
- Require a Pod restart after the external Secret is rotated.

SSH key generation:

- Run a pre-install/pre-upgrade Helm Hook and Argo CD `PreSync` Job when `ssh.keyGeneration.enabled=true`.
- Use the pinned Tabminal image, which includes Node.js and `ssh-keygen`, rather than adding another mutable image dependency.
- Generate `id_ed25519` and `id_ed25519.pub` only when the configured output Secret does not exist.
- When the output Secret exists, decrypt its private key with the configured passphrase and compare the derived public key with the stored public key.
- Fail without changing the Secret when the passphrase, key type, or public key does not match.
- Never print secret values, public key content, or a passphrase hash. Log only the namespace, Secret name, action, and key names.
- Do not set an owner reference from the generated Secret to the Hook Job. The Secret must remain stable across Job cleanup, Helm renders, Argo CD syncs, and uninstall operations.
- Require the passphrase Secret to exist before an Argo CD sync. A `PreSync` Hook cannot consume a Secret that is first created later in the same sync.
- Require a full Argo CD sync because selective sync does not execute hooks.
- Use a dedicated ServiceAccount with a projected token. Make its lifetime configurable with a minimum and default of 600 seconds.
- Grant `get` only for the target Secret name and namespace-wide `create` for Secrets. Standard Kubernetes RBAC cannot restrict a top-level `create` request by `resourceNames`.
- Do not grant `list`, `watch`, `update`, `patch`, or `delete`.

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

The chart does not template credential Secrets. `tabminal.existingSecret` is required and its password key is configured with `tabminal.existingSecretPasswordKey`. Optional credential key selectors include:

- `tabminal.existingSecretOpenrouterKey`
- `tabminal.existingSecretOpenaiKey`
- `tabminal.existingSecretGoogleKey`
- `tabminal.existingSecretGoogleCx`
- `tabminal.existingSecretCloudflareKey`

The Deployment reads configured credentials with `valueFrom.secretKeyRef`.

The optional SSH key generation Job creates one runtime `Opaque` Secret containing `id_ed25519` and `id_ed25519.pub`. This generated output is not a chart-rendered credential Secret and is intentionally not owned by the Hook Job.

### Extra Deploy

Include `templates/extra-list.yaml` so users can add NetworkPolicy, middleware, additional Secrets, or controller-specific resources without forking the chart.

## Security Considerations

Tabminal is high privilege by design: it provides browser access to a real shell and host-local files inside the container.

Chart documentation must make these constraints explicit:

- Require `tabminal.acceptTerms=true` before the application starts.
- Require a stable password for normal deployments; do not rely on the generated temporary password in Kubernetes.
- Require `existingSecret` for credentials and reject credentials in Helm values.
- Keep ingress disabled by default.
- Recommend an external access-control layer for any remote access.
- Treat the PVC as sensitive because it can contain session snapshots, auth state, cluster host registry, agent settings, and command context.
- Treat SSH Secrets and passphrases as privileged credentials. Keep the mounted runtime copy in memory rather than the PVC.
- Document that a short passphrase provides limited offline protection and that the generated Secret persists until explicitly deleted.
- Warn that AI keys may send terminal context, file paths, or command history to the configured provider, consistent with upstream behavior.

## Initial Implementation Plan

1. Create `application/tabminal/chart` by adapting the `yacd` chart structure.
2. Add `Chart.yaml` with the Bitnami `common` dependency and upstream Tabminal metadata.
3. Add `values.yaml` with image, port, Tabminal config, service, ingress, persistence, probes, resources, and extra deploy values.
4. Require a pre-existing Secret for the password and optional provider credentials.
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
  existingSecret: tabminal-secret
  existingSecretPasswordKey: password

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

- Decide whether to enforce `replicas=1` with a template failure or only document it as the safe default.
