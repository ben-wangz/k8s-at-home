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
