# chromium-bridge implementation plan

## 1. Goal

`chromium-bridge` provides a collaborative browser runtime for automation and manual takeover:

- Automation connects via CDP (Playwright `connectOverCDP`)
- Human operators connect via noVNC
- Both access the same browser session

Target deployment model:

- Kubernetes + Helm chart
- Two independent container images
- Chart wiring based on Bitnami `common` library

## 2. Scope and constraints

### Must-have

1. `server` and `novnc` are separate images
2. Helm chart wires both components together
3. Reuse patterns from `application/podman-in-container/`
4. Use Bitnami `common` dependency heavily (naming, labels, images, affinity, resources, ingress API compatibility)

### Out of scope (first iteration)

- Multi-tenant browser pool
- Session scheduling/orchestration
- Advanced auth gateway

## 3. High-level architecture

Components:

1. `chromium-bridge-server`
   - Runs Chrome/Chromium for Testing (CFT), Xvfb, x11vnc, socat
   - CDP internal listener: `127.0.0.1:9223`
   - CDP exposed listener: `0.0.0.0:9222` (via socat)
   - VNC listener: `0.0.0.0:5900`

2. `chromium-bridge-novnc`
   - Runs websockify (+ noVNC static assets)
   - Exposes web endpoint: `0.0.0.0:6080`
   - Forwards websocket traffic to `chromium-bridge-server` VNC endpoint (`5900`)

Traffic model:

- Playwright/agents -> `server-service:9222` (CDP)
- Humans -> `novnc-service:6080` (web UI) -> `server-service:5900`

## 4. Repository layout

Planned structure:

```text
application/chromium-bridge/
  README.md
  IMPLEMENTATION_PLAN.md
  server/
    Containerfile
    start.sh
    VERSION
  novnc/
    Containerfile
    start.sh
    VERSION
  chart/
    Chart.yaml
    values.yaml
    templates/
      _helpers.tpl
      server-deployment.yaml
      server-service.yaml
      novnc-deployment.yaml
      novnc-service.yaml
      novnc-ingress.yaml
      extra-list.yaml
```

## 5. Container image design

### 5.1 server image

Base image:

- `m.daocloud.io/docker.io/library/ubuntu:24.04`

Responsibilities:

- Install runtime deps (`xvfb`, `x11vnc`, `fluxbox`, `socat`, fonts, browser libs)
- Install `tini` as PID 1 init process
- Download CFT from `files.m.daocloud.io/.../chrome-for-testing-public/...`
- Start services in one process script:
  - `Xvfb`
  - `fluxbox`
  - `x11vnc`
  - `chrome --remote-debugging-address=127.0.0.1 --remote-debugging-port=9223`
  - `socat TCP-LISTEN:9222 -> TCP:127.0.0.1:9223`

Process lifecycle requirements:

- Container entrypoint uses `tini -- /start.sh`
- `start.sh` traps `SIGTERM`/`SIGINT`, terminates child processes, then `wait`s
- Ensure zombie reaping and graceful Pod shutdown behavior

Exposed ports:

- `9222` (CDP external)
- `5900` (VNC)

### 5.2 novnc image

Base image suggestion:

- `m.daocloud.io/docker.io/library/python:3.12-slim`

Responsibilities:

- Provide websockify runtime
- Provide noVNC frontend static files
- Launch websockify bound to `6080`
- Forward to configured target (`SERVER_HOST:SERVER_VNC_PORT`)

Exposed port:

- `6080` (HTTP/websocket entry)

## 6. Helm chart design

Chart metadata (`chart/Chart.yaml`):

- `apiVersion: v2`, `type: application`
- `name: chromium-bridge`
- `dependencies` includes Bitnami `common` (`2.x.x`)
- `annotations` includes custom image metadata for version tooling

### 6.1 values model

Top-level common settings:

- `replicaCount`
- `commonLabels`, `commonAnnotations`
- `podLabels`, `podAnnotations`
- `imagePullSecrets`
- `affinity`, `nodeSelector`, `tolerations`
- `resourcesPreset` style compatibility

`server` block:

- `enabled`
- `image.repository/tag/pullPolicy`
- `ports.cdp`, `ports.vnc`, `ports.cdpInternal`
- `env` (resolution, timezone, CFT overrides)
- `service` (type, ports, annotations, nodePort optional)
- `persistence.userData` (optional PVC or emptyDir)
- probes and resources

`novnc` block:

- `enabled`
- `image.repository/tag/pullPolicy`
- `port` (`6080`)
- `target.host`, `target.port` (default host generated from helper)
- `service` (type, ports, annotations)
- `ingress` (className, host, path, tls)
- probes and resources

`extraDeploy` for custom manifests.

### 6.2 templates and helper strategy

Helpers (`_helpers.tpl`):

- `chromium-bridge.server.fullname`
- `chromium-bridge.novnc.fullname`
- `chromium-bridge.server.serviceName`
- `chromium-bridge.novnc.serviceName`

Bitnami `common` usage (core):

- `common.capabilities.deployment.apiVersion`
- `common.capabilities.ingress.apiVersion`
- `common.names.fullname`, `common.names.namespace`
- `common.labels.standard`, `common.labels.matchLabels`
- `common.images.image`
- `common.affinities.pods`, `common.affinities.nodes`
- `common.resources.preset`
- `common.tplvalues.render`

Wiring:

- `novnc-deployment` resolves server target host via helper output
- `novnc` starts with target `${SERVER_HOST}:${SERVER_VNC_PORT}`
- `server-service` publishes both `cdp` and `vnc` ports

## 7. Service and exposure strategy

Default:

- `server-service`: `ClusterIP`
  - `9222` named `cdp`
  - `5900` named `vnc`
- `novnc-service`: `ClusterIP`
  - `6080` named `http`

Optional:

- NodePort/LoadBalancer exposure for `novnc-service`
- Ingress only for noVNC web endpoint

## 8. Security baseline

First iteration baseline:

- Run with least privileges possible while preserving browser functionality
- Keep CDP service internal by default (`ClusterIP`)
- Expose noVNC externally only when explicitly configured
- Avoid embedding static credentials in chart defaults

Future hardening:

- noVNC auth integration
- network policy examples
- optional TLS termination profile

## 9. Operational behavior

Health checks:

- `server`: TCP probe on `9222`
- `novnc`: TCP or HTTP probe on `6080`

Rolling behavior:

- `Deployment` strategy configurable via values
- default can be `RollingUpdate`

Observability:

- container logs for startup and forwarding status
- explicit startup banner with effective ports

## 10. Acceptance criteria

MVP is done when:

1. `helm install` creates both deployments and services
2. `curl http://<server-svc>:9222/json/version` returns valid CDP metadata
3. Playwright `connectOverCDP` works against server service
4. noVNC web endpoint is reachable and shows live browser display
5. Manual actions via noVNC are visible in the same browser session used by Playwright

## 11. Delivery phases

Phase 1 (MVP):

- Create two images and chart skeleton
- Wire service-to-service connection
- Provide install docs and verification steps

Phase 2 (stability):

- Tune probes/resources
- Add optional persistence for browser profile
- Improve startup diagnostics

Phase 3 (hardening):

- Access controls and network policies
- TLS and ingress profiles
- Optional multi-instance patterns
