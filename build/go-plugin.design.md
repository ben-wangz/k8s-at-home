# sshpiper Go Plugin Design

## Scope

This document defines a single-tenant custom Go plugin design for `sshpiper`.

Goals:

- Keep the existing username routing protocol.
- Support two mutually exclusive runtime modes:
  - `password` mode
  - `key` mode
- Keep the chart and plugin behavior predictable and explicit.

Non-goals:

- No transparent forwarding of a client private key to the upstream SSH server.
- No mixed auth behavior inside one runtime mode.

## Routing Protocol

The downstream username remains the routing input:

```text
<upstream-user>+<target>
<upstream-user>+<target>+<target-port>
```

Parsing rules:

1. Split by `+`.
2. Accept only 2 or 3 segments.
3. Segment 1 is `upstreamUser`.
4. Segment 2 is `target`.
5. Segment 3, if present, is `port`; otherwise default to `22`.
6. Reject invalid or out-of-range ports.

## Runtime Modes

The plugin supports exactly one mode per deployment.

### Mode: `password`

Behavior:

- Downstream authentication: password only.
- Upstream authentication: password only.
- The password provided by the client is forwarded to the upstream server.
- The plugin does not load or use any upstream private key material.

Security property:

- `sshpiper` does not persist upstream private keys.
- The user password is still processed in memory during the connection flow.

### Mode: `key`

Behavior:

- Downstream authentication: public key only.
- Downstream password authentication is disabled.
- Upstream authentication: private key only.
- The plugin selects and uses configured upstream private key material.
- Password forwarding is disabled.

Security property:

- Password login is fully disabled on both downstream and upstream paths.
- `sshpiper` must have access to the required upstream private key material.

## Mode Selection Rules

1. The deployment must declare one explicit `authMode`.
2. Allowed values:
   - `password`
   - `key`
3. The plugin must fail startup if `authMode` is missing or invalid.
4. The plugin must not auto-fallback from one mode to the other.

## Target Validation

The plugin validates the parsed `target` against a static allowlist.

Example:

```yaml
allowedTargets:
  - gitea-ssh.basic-components
  - "*.codespace"
```

Rules:

- Allowlist is validation-only.
- The parsed target is never rewritten.
- Exact entries must match exactly.
- Wildcard entries only support suffix-style validation such as `*.codespace`.
- Non-allowlisted targets must be rejected.

## Authentication Flow

### Password Mode

1. Client connects to `sshpiper`.
2. Plugin parses username route.
3. Plugin validates `target` and `port`.
4. Plugin advertises only `password` as supported auth method.
5. Client submits password.
6. Plugin creates upstream connection using:
   - `host = <target>:<port>`
   - `username = <upstream-user>`
   - `password = downstream password`

### Key Mode

1. Client connects to `sshpiper`.
2. Plugin parses username route.
3. Plugin validates `target` and `port`.
4. Plugin advertises only `publickey` as supported auth method.
5. Client presents public key.
6. Plugin validates the client public key against configured authorized keys.
7. Plugin creates upstream connection using:
   - `host = <target>:<port>`
   - `username = <upstream-user>`
   - `private_key_data = configured upstream private key`

## Key Mode Data Model

Key mode needs local configuration data for two independent purposes.

### Downstream authorization

Used to decide which client public keys may log in to `sshpiper`.

Required data:

- `authorized_keys`

Semantics:

- If the presented client key is not listed, reject the connection.
- This file controls access to the proxy itself.

### Upstream authentication

Used by `sshpiper` to authenticate to the upstream SSH server.

Required data:

- `id_ed25519` or another supported SSH private key file

Semantics:

- This key is not derived from the client session.
- This key represents the proxy when logging into the upstream server.

## Host Key Verification

Host key verification is independent from auth mode.

Supported policies:

- `strict`
- `tofu`
- `insecure`

Default:

- `tofu`

### `strict`

- Upstream host key must already exist in `known_hosts`.
- Unknown or changed host keys are rejected.

### `tofu`

- First-seen upstream host key is accepted and recorded.
- Changed host keys are rejected unless explicitly updated by an operator.

### `insecure`

- Host key verification is skipped.
- For testing only.

Default recommendation:

- Default to `tofu`.
- Use `strict` when upstream host keys are fully managed in advance.
- Do not use `insecure` in production.

## Config Surface

Minimum plugin configuration:

```yaml
auth:
  mode: password
  allowedTargets:
    - gitea-ssh.basic-components
    - "*.codespace"
  hostKeyPolicy: tofu
storage:
  knownHosts:
    type: emptyDir
    existingClaim: ""
```

For `key` mode, additional mounted files are required:

- `authorized_keys`
- upstream private key file
- optional `known_hosts`

Chart-facing simplification:

- downstream `authorized_keys` should be maintained as a values array and rendered into a dedicated Secret
- upstream SSH key material should be stored in a separate Secret
- the upstream Secret only needs to contain the configured private key entry
- the plugin only loads the upstream private key file at runtime

## Implementation Plan

### Chart direction

- Stop using the Lua plugin and mounted `routing.lua` script.
- Stop exposing old Lua script and plugin selector values.
- Run the custom Go plugin binary through the image entrypoint.
- Keep the existing listener and server host key handling.

### Repository layout

The implementation should use these repository paths:

- Go plugin source: `application/sshpiper/src/`
- Container files: `application/sshpiper/container/`
- Image build context root: `application/sshpiper/`
- Helm chart: `application/sshpiper/chart/`

### Plugin startup contract

The chart should provide the following runtime inputs to the custom plugin:

- `SSHPIPER_ALLOWED_TARGETS`
- `SSHPIPER_AUTH_MODE`
- `SSHPIPER_HOSTKEY_POLICY`
- `SSHPIPER_KNOWN_HOSTS_PATH`
- file paths for:
  - downstream `authorized_keys` via `SSHPIPER_AUTHORIZED_KEYS_PATH`
  - upstream private key via `SSHPIPER_UPSTREAM_PRIVATE_KEY_PATH`

Recommended defaults:

- known hosts path: `/var/sshpiper/known_hosts`
- downstream authorized keys path: `/auth/authorized_keys`
- upstream private key path: `/upstream-keypair/id_ed25519`

### Plugin packaging

The upstream `tg123/sshpiperd` image does not include this custom plugin binary.

Implementation requirement:

- Build and ship a custom container image using `application/sshpiper/container/Containerfile`.
- Base the image on the upstream official sshpiper image.
- Add the custom plugin binary built from `application/sshpiper/src/`.
- Add an internal `entrypoint.sh` that starts `sshpiperd` with the bundled plugin path.
- The final image must contain:
  - `/sshpiperd/sshpiperd`
  - `/sshpiperd/plugins/routeauth`
- The chart must rely on the image entrypoint instead of passing an explicit plugin command.

Implication:

- Removing the Lua path is an intentional break from the current upstream-image-only deployment model.
- The deployment becomes runnable again only after the custom plugin binary is packaged into the derived image.

### Container build strategy

Recommended approach:

1. Compile the Go plugin from `application/sshpiper/src/`.
2. Use a multi-stage container build in `application/sshpiper/container/` with build context `application/sshpiper/`.
3. Use the official upstream sshpiper image as the runtime base stage.
4. Copy the compiled plugin binary into `/sshpiperd/plugins/routeauth`.
5. Set the image entrypoint to start `sshpiperd` with that plugin.

Expected result:

- Keep upstream runtime layout and entry binary unchanged.
- Only add the custom plugin binary required by this repository design.

### Known hosts storage

The chart should support one writable location for `known_hosts` state.

Supported modes:

1. `emptyDir`
   - default
   - suitable for `tofu` when persistence across pod recreation is not required
2. PVC
   - mount one PVC for persistent `known_hosts` state
   - recommended when `tofu` state must survive pod rescheduling or restart

Configuration shape:

```yaml
storage:
  knownHosts:
    enabled: false
    existingClaim: ""
```

Rules:

- When `enabled=false`, mount an `emptyDir` volume to the known hosts working path.
- When `enabled=true` and `existingClaim` is empty, the chart should create a PVC and mount it.
- When `enabled=true` and `existingClaim` is set, mount that PVC.
- The mount path stays the same across both modes.
- The plugin writes runtime host key state only to this path.

### Interaction with host key policies

- `tofu`
  - default policy
  - uses the writable known hosts path
  - works with either `emptyDir` or PVC
- `strict`
  - user may use the chart-provided known hosts storage, or may fully override file sourcing with `extraVolumes` and `extraVolumeMounts`
  - no automatic learning should occur in strict mode
- `insecure`
  - no host key validation is performed
  - the presence of `emptyDir` or PVC does not affect behavior

### Removal of Lua-specific behavior

The implementation should remove all Lua-specific chart behavior:

- mounted `ConfigMap` for `routing.lua`
- `config.script`
- `config.plugin=lua`
- Lua-specific deployment args such as `--script /config/routing.lua`

### Minimal chart values after cleanup

```yaml
auth:
  mode: password
  allowedTargets:
    - gitea-ssh.basic-components
    - "*.codespace"
  hostKeyPolicy: tofu
  authorizedKeys:
    values: []
    existingSecret: ""
    secretName: ""
    secretKey: authorized_keys
    mountPath: /auth
    filePath: /auth/authorized_keys
  upstreamKeypair:
    existingSecret: ""
    secretName: ""
    mountPath: /upstream-keypair
    privateKeyPath: /upstream-keypair/id_ed25519
    privateKeyKey: id_ed25519

storage:
  knownHosts:
    enabled: false
    existingClaim: ""
```

## File Requirements

When `key` mode is enabled:

- `authorized_keys` must be readable by the `sshpiper` process.
- Upstream private key file must be readable by the `sshpiper` process.
- Recommended permissions:
  - private key: `0400`
  - `authorized_keys`: `0400`
  - `known_hosts`: `0400`

## Error Handling

The plugin must reject the connection when:

- username format is invalid
- target is not allowlisted
- port is invalid
- auth method does not match the configured mode
- `key` mode client key is unauthorized
- required key-mode files are missing
- upstream host key policy fails

The plugin should log the rejection reason without logging secrets.

## Operational Tradeoffs

### Password mode

Pros:

- Simplest deployment model
- No upstream private key storage in `sshpiper`
- Closest to current password forwarding behavior

Cons:

- Relies on password-based upstream access
- Weaker long-term credential posture than key-based auth

### Key mode

Pros:

- Disables password auth completely
- Stronger SSH-native operational model
- Better fit for hardened upstream access

Cons:

- `sshpiper` must hold upstream private key material
- Requires explicit client key authorization data

## Recommendation

Design the plugin and chart around one explicit switch:

- `authMode=password`
- `authMode=key`

This keeps behavior simple:

- password mode means password end-to-end
- key mode means key end-to-end, with no password fallback

That separation matches the intended security model and avoids ambiguous mixed behavior.
