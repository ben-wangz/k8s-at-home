# Clash Console

Clash Console is the static browser UI shipped in the Clash chart. Mihomo serves it at `/ui/` on the controller endpoint. The browser calls Mihomo directly for proxy inventory and selection, traffic, connections, and live logs.

## Access and authentication

With the default NodePort values, open `http://<node-address>:31909/ui/`. With controller ingress, open `https://<controller-host>/ui/`. Ingress must route `/` to the controller so that UI assets, APIs, and WebSocket upgrades use the same origin.

Configure authentication with an existing Secret containing a nonempty, single-line `secret` value:

```yaml
controller:
  existingSecret: clash-controller
  secretKey: secret
```

The init container reads that Secret into the runtime Mihomo configuration. Static assets contain no credentials. Enter the same secret in the Console connection form. Credentials last only for the current page session; reloading requires entering them again.

For HTTPS ingress with an existing TLS Secret:

```yaml
ingress:
  controller:
    enabled: true
    hostname: controller.example.com
    path: /
    tls: true
    secretName: controller-tls
```

When cert-manager annotations are used without `secretName`, the existing `<hostname>-controller-tls` naming convention applies. TLS termination must support WebSocket upgrades. API streams use `wss` when the controller URL uses HTTPS.

The default controller URL is the Console's origin. For a different controller origin, enter its browser-reachable URL and configure that controller's allowed origins. For this chart:

```yaml
controller:
  cors:
    allowedOrigins:
      - https://console.example.com
    allowPrivateNetwork: false
```

Cross-origin behavior also depends on browser network policies. An HTTPS Console requires an HTTPS controller. Kubernetes service DNS alone is insufficient for a browser outside the cluster.

## Views

- **Proxies:** inspect groups and members, filter names, and change selector groups. Successful changes are confirmed by reading the group from the controller. Automatic groups are displayed without manual selection controls. A switch affects new connections; existing connections keep their routes.
- **Connections:** view live snapshots, destinations, sources, protocol, proxy chains, rules, start times, and byte counts. Missing optional metadata is displayed as unavailable. Filtering includes process information when supplied.
- **Live logs:** subscribe to new controller events, select a severity threshold, filter messages, pause display, clear the local buffer, or disable automatic scrolling. The buffer retains at most 1,000 events. Pausing freezes display while the buffer continues receiving events. Changing severity restarts the stream. Logs have no historical replay.

Traffic cards show controller-reported rates and totals. Interrupted streams retain their last data with a stale indicator. Reconnection uses delays from one second up to 30 seconds. Changing controllers closes old streams and clears their data. A rejected credential stops reconnection until the user connects again.

Selection survives a browser reload because it belongs to Mihomo. To retain selection and provider caches across Pod replacement, enable `persistence.enabled` or provide `persistence.existingClaim`. `config.storeSelected` defaults to `true`. With the default ephemeral storage, Pod replacement can reset selection.

## Build and packaging

The frontend uses TypeScript, native DOM modules, and Vite. From any directory, use an absolute application path:

```bash
CLASH_APP=/absolute/path/to/application/clash
npm --prefix "$CLASH_APP/console/frontend" ci
npm --prefix "$CLASH_APP/console/frontend" run build
```

The build creates `console/frontend/dist/` and updates the chart's tracked `files/console/` assets and `SHA256SUMS`. Assets use `/ui/` as their base path, and navigation uses URL fragments so reloads work with Mihomo's file server. Commit regenerated chart assets with frontend changes. The chart can be packaged without Node.js because it already contains the built UI.

`console.enabled` defaults to `true`. Set it to `false` to disable UI mounting and hosting. `console.mountPath` defaults to `/root/.config/mihomo/ui`, below Mihomo's configuration directory so the pinned core accepts it as a safe path. If `config.dir` changes, set `console.mountPath` to a directory below it. A changed bundled ConfigMap triggers a Pod rollout through its checksum annotation.

For externally supplied assets, set `console.existingConfigMap` to a ConfigMap containing `index.html` and all referenced asset filenames as flat keys. Mounting replaces the bundled assets. Roll out the Deployment when changing an external ConfigMap or controller Secret.

For local development, run `npm --prefix "$CLASH_APP/console/frontend" run dev`, open the Vite `/ui/` URL, and enter a controller endpoint configured to allow that origin.

## Migration from YACD

1. Build or obtain the updated Clash chart and enable the Console.
2. Open the existing controller endpoint's `/ui/` path and verify proxy selection, connections, and logs.
3. Update bookmarks and any links that previously pointed to the separate YACD hostname.
4. Remove the old YACD Helm release explicitly after its users have migrated.

The existing `application/yacd` chart remains available during migration. Installing or upgrading Clash does not remove another Helm release.

See the [test suite guide](../tests/README.md) for independent regression runs and the [requirements](todo/clash-console-requirements.md) for acceptance mappings.
