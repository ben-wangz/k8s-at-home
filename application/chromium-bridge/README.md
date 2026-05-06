# chromium-bridge

`chromium-bridge` provides a shared Chromium runtime for automation and manual visual takeover.

- Automation clients connect through CDP (`server`)
- Human operators connect through noVNC (`novnc`)

## Quick start

1. Build images from `application/chromium-bridge/container/server` and `application/chromium-bridge/container/novnc`.
2. Set image repository/tag in `application/chromium-bridge/chart/values.yaml`.
3. Deploy chart:

```bash
helm upgrade --install chromium-bridge application/chromium-bridge/chart \
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
