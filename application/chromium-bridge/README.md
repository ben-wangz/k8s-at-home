# chromium-bridge

`chromium-bridge` provides a shared Chromium runtime for both automation (CDP/Playwright) and human visual takeover (noVNC).

## Components

- `server`: Chromium(CFT) + Xvfb + x11vnc + socat
- `novnc`: websockify/noVNC web endpoint
- `chart`: Helm chart wiring server and novnc together

## Current phase

Phase 1 (MVP scaffold) is implemented:

- Two independent image contexts
- One Helm chart using Bitnami `common` dependency
- Service-to-service wiring (`novnc` -> `server:5900`)

## Layout

```text
application/chromium-bridge/
  IMPLEMENTATION_PLAN.md
  README.md
  server/
  novnc/
  chart/
```
