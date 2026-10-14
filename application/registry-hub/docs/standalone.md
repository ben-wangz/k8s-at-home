# Standalone deployment

Run the backend and Distribution as separate processes on one host, with persistent directories for each. The Kubernetes initialization commands require the Kubernetes API; they do not initialize standalone files.

## Prepare the host

Install the Go toolchain required by [src/go.mod](../src/go.mod), Python 3, OpenSSL, and `htpasswd`. Provide a Distribution executable matching the image version pinned in [chart/values.yaml](../chart/values.yaml). The example assumes that executable is `/opt/registry-hub/bin/registry` and that the service account can write `/srv/registry-hub`. Shell examples use Bash.

```sh
REPO_ROOT=/absolute/path/to/k8s-at-home
HUB_ROOT=/srv/registry-hub
DISTRIBUTION_BIN=/opt/registry-hub/bin/registry
umask 077
mkdir -p "$HUB_ROOT/bin" "$HUB_ROOT/secrets" \
  "$HUB_ROOT/backend" "$HUB_ROOT/registry"
go build -C "$REPO_ROOT/application/registry-hub/src" \
  -o "$HUB_ROOT/bin/registry-hub" ./cmd/registry-hub
```

Run the following credential creation once. Existing files are preserved by exclusive creation; keep them across restarts:

```sh
set -euo pipefail
umask 077
python3 - "$HUB_ROOT/secrets" <<'PY'
import json, pathlib, secrets, sys
root = pathlib.Path(sys.argv[1])
files = {
    "admin.json": json.dumps({"basic": [{
        "username": "admin", "password": secrets.token_hex(32),
        "permissions": ["task-admin", "referrer-prune", "gc-admin"]
    }]}),
    "distribution.json": json.dumps({
        "username": "registry-hub-backend", "password": secrets.token_hex(32)
    }),
    "http-secret": secrets.token_hex(32)
}
if any((root / name).exists() for name in files):
    raise SystemExit("Credential files already exist; reuse them.")
for name, content in files.items():
    with (root / name).open("x") as output:
        output.write(content)
PY

python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))["password"])' \
  "$HUB_ROOT/secrets/distribution.json" |
  htpasswd -iBc "$HUB_ROOT/secrets/htpasswd" registry-hub-backend

openssl req -x509 -newkey rsa:3072 -nodes -days 365 \
  -keyout "$HUB_ROOT/secrets/tls.key" \
  -out "$HUB_ROOT/secrets/tls.crt" \
  -subj /CN=localhost \
  -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
  -addext 'extendedKeyUsage=serverAuth'
```

The htpasswd and TLS commands are also initial setup steps. Preserve their files on restart; manage certificate renewal before expiry. The self-signed certificate is trusted only for this internal localhost endpoint.

## Start Distribution

Save this configuration as `/srv/registry-hub/distribution.yaml`. All paths match the directories above:

```yaml
version: 0.1
log:
  level: info
storage:
  filesystem:
    rootdirectory: /srv/registry-hub/registry
  delete:
    enabled: true
  redirect:
    disable: true
  maintenance:
    uploadpurging:
      enabled: false
    readonly:
      enabled: false
validation:
  manifests:
    urls:
      allow:
        - '^https?://'
    indexes:
      platforms: all
auth:
  htpasswd:
    realm: registry-hub-internal
    path: /srv/registry-hub/secrets/htpasswd
http:
  addr: 127.0.0.1:5001
  secret: ""
  tls:
    certificate: /srv/registry-hub/secrets/tls.crt
    key: /srv/registry-hub/secrets/tls.key
```

Start it in a terminal with the setup variables above:

```sh
REGISTRY_HTTP_SECRET="$(cat "$HUB_ROOT/secrets/http-secret")" \
  "$DISTRIBUTION_BIN" serve "$HUB_ROOT/distribution.yaml"
```

Binding Distribution to loopback keeps it private. The HTTP secret is persisted in its protected file and reused for upload sessions.

## Start the backend

In another terminal:

```sh
HUB_ROOT=/srv/registry-hub
export REGISTRY_HUB_CLIENT_ADDR=127.0.0.1:5000
export REGISTRY_HUB_ADMIN_ADDR=127.0.0.1:8081
export REGISTRY_HUB_STORAGE_BACKEND=filesystem
export REGISTRY_HUB_FILESYSTEM_ROOT="$HUB_ROOT/backend"
export REGISTRY_HUB_DISTRIBUTION_ENDPOINT=https://localhost:5001
export REGISTRY_HUB_DISTRIBUTION_CA_FILE="$HUB_ROOT/secrets/tls.crt"
export REGISTRY_HUB_DISTRIBUTION_AUTH_CONFIG="@$HUB_ROOT/secrets/distribution.json"
export REGISTRY_HUB_ADMIN_AUTH_CONFIG="@$HUB_ROOT/secrets/admin.json"
export REGISTRY_HUB_GC_MODE=disabled

"$HUB_ROOT/bin/registry-hub"
```

An `@` prefix makes a JSON configuration variable read from a file. Client authentication is disabled in this localhost example. To enable it, set `REGISTRY_HUB_CLIENT_AUTH_CONFIG=@/absolute/path/to/client-auth.json` using the [credential format](security.md#client-and-admin-credential-files).

Check from another terminal:

```sh
curl --fail --silent --show-error http://127.0.0.1:8081/readyz
curl --fail --silent --show-error http://127.0.0.1:5000/v2/
```

Both return `200` after startup. Use `127.0.0.1:5000` with the HTTP client options in [Usage](usage.md). For remote users, put an HTTPS reverse proxy in front of the client listener and enable client authentication. Keep the admin listener and Distribution private.

To use the authenticated API documentation and the commands in [Operations](operations.md), prepare a temporary netrc file from the local admin credential:

```sh
set -euo pipefail
HUB_ROOT=/srv/registry-hub
ADMIN_URL=http://127.0.0.1:8081
umask 077
ADMIN_NETRC="$(mktemp)"
python3 -c '
import json, sys
credential = json.load(open(sys.argv[1]))["basic"][0]
sys.stdout.write("machine 127.0.0.1 login {username} password {password}\n".format(
    **credential))
' "$HUB_ROOT/secrets/admin.json" > "$ADMIN_NETRC"
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  "$ADMIN_URL/openapi.yaml"
```

Remove the temporary file with `rm -- "$ADMIN_NETRC"` when finished.

## Local GC

Keep GC disabled until the backend is the only Registry/storage writer and Distribution upload purging is disabled. Stop the backend, add these variables to its startup environment, and restart it with the same settings and files:

```sh
export REGISTRY_HUB_GC_MODE=local
export REGISTRY_HUB_GC_EXECUTABLE=/opt/registry-hub/bin/registry
export REGISTRY_HUB_GC_CONFIG=/srv/registry-hub/distribution.yaml
export REGISTRY_HUB_GC_UNIQUE_WRITER_CONFIRMED=true
export REGISTRY_HUB_GC_UPLOAD_PURGING_DISABLED=true
```

The executable and configuration must use absolute paths, the same Distribution version, and the exact live storage location. GC invokes `registry garbage-collect <config>` through the backend's write gate. Trigger, monitor, and recover it through [Operations](operations.md#garbage-collection).

For persistent operation, supervise both processes, preserve the directories and credentials, start Distribution before the backend, and stop the backend before Distribution. SIGTERM allows the backend to stop admitting work and persist recovery state. Apply the [backup procedure](operations.md#backup-and-restore) to both storage directories and the secret files.
