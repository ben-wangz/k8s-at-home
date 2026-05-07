# chromium-bridge

`chromium-bridge` provides a shared Chromium runtime for automation and manual visual takeover.

- Automation clients connect through CDP (`server`)
- Human operators connect through noVNC (`novnc`)

## Quick start

1. Get the latest chart version:

```bash
export CHART_VERSION=$(forgekit --project-root ../.. version get chart chromium-bridge)
```

2. Deploy chart from GHCR (uses published images by default):

```bash
helm upgrade --install chromium-bridge oci://ghcr.io/ben-wangz/k8s-at-home-charts/chromium-bridge \
  --atomic \
  --version ${CHART_VERSION} \
  --namespace chromium-bridge \
  --create-namespace
```

## Verify

- Check CDP endpoint:

```bash
curl -sS http://<server-host>:9222/json/version
```

- Check noVNC endpoint:

```bash
curl -I http://<novnc-host>:6080/
```

## CDP client note

When using Playwright or other CDP clients, forward the CDP service to a local port (for example `127.0.0.1:9222`) and connect through localhost.

If you connect with a domain host directly, Chromium may reject it with:

`Host header is specified and is not an IP address or localhost`

Example:

```bash
kubectl -n chromium-bridge port-forward svc/chromium-bridge-server 9222:9222
```

## Ingress examples

- Enable CDP ingress:

```bash
helm upgrade --install chromium-bridge oci://ghcr.io/ben-wangz/k8s-at-home-charts/chromium-bridge \
  --version ${CHART_VERSION} \
  --namespace chromium-bridge \
  --create-namespace \
  --set server.ingress.enabled=true \
  --set server.ingress.ingressClassName=traefik \
  --set server.ingress.hostname=chromium-bridge-cdp.example.com
```

- Enable noVNC ingress:

```bash
helm upgrade --install chromium-bridge oci://ghcr.io/ben-wangz/k8s-at-home-charts/chromium-bridge \
  --version ${CHART_VERSION} \
  --namespace chromium-bridge \
  --create-namespace \
  --set novnc.ingress.enabled=true \
  --set novnc.ingress.ingressClassName=traefik \
  --set novnc.ingress.hostname=chromium-bridge-novnc.example.com
```
