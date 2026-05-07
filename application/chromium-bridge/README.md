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
