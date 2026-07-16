# cli-proxy-api

`cli-proxy-api` deploys CLIProxyAPI with an OAuth credential store and a
Traefik-compatible Kubernetes Ingress.

The chart uses a small runtime image derived from the upstream CLIProxyAPI
image. The derived image adds only `jq` and two operational scripts:

- `cli-proxy-api-render-config` merges non-sensitive chart configuration with
  API keys from a Kubernetes Secret.
- `cli-proxy-api-xai-login` runs the interactive xAI device flow and updates
  the resulting OAuth metadata without printing tokens.

## Prerequisites

- Kubernetes with a default StorageClass, or an existing PVC
- Helm
- kubectl, jq, and OpenSSL on the operator workstation
- A pre-created Secret containing one or more CPA client API keys
- A Traefik Ingress Controller when `ingress.enabled=true`
- A browser logged in to the xAI account used for device authorization

The device authorization step requires manual interaction after the chart is
installed. It is not implemented as a Helm hook or Job.

## Prepare credentials

The Secret contains only an `api-keys` field. Its value is a JSON array of
non-empty strings. It does not contain the complete CLIProxyAPI configuration.

Create a namespace and a Secret without putting the API key in a values file:

```bash
export NAMESPACE=basic-components
export CREDENTIALS_SECRET=cli-proxy-api-credentials

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml \
  | kubectl apply -f -

CPA_API_KEY="$(openssl rand -hex 32)"
jq -cn --arg key "$CPA_API_KEY" '[$key]' \
  | kubectl create secret generic "$CREDENTIALS_SECRET" \
      --namespace "$NAMESPACE" \
      --from-file=api-keys=/dev/stdin \
      --dry-run=client -o yaml \
  | kubectl apply -f -
```

Keep `CPA_API_KEY` in the client secret manager that will use the proxy. Do not
commit it to Git or add it to Helm values. Clear it from the current shell when
it is no longer needed:

```bash
unset CPA_API_KEY
```

To provide multiple client keys, build a JSON array with additional `--arg`
parameters and update the same Secret.

## Configure values

Create a non-sensitive values file:

```yaml
credentials:
  existingSecret: cli-proxy-api-credentials

persistence:
  auth:
    enabled: true
    storageClass: ""
    size: 1Gi

ingress:
  enabled: true
  ingressClassName: traefik
  hostname: cli-proxy-api.example.internal
  annotations:
    traefik.ingress.kubernetes.io/router.entrypoints: websecure
    traefik.ingress.kubernetes.io/router.tls: "true"
  tls: true
  tlsSecret: cli-proxy-api-tls
```

TLS certificates and Traefik entryPoints are managed outside this chart. Set
the annotations and TLS reference required by the existing Traefik deployment.

The application, Service, and probe port is fixed at `8317` and is not exposed
as a values option.

## Install

Get the chart version:

```bash
export CHART_VERSION="$(
  forgekit --output json --project-root ../.. version get cli-proxy-api \
    | jq -r '.data.app.value'
)"
```

Install from the OCI registry:

```bash
helm upgrade --install cli-proxy-api \
  oci://ghcr.io/ben-wangz/k8s-at-home-charts/cli-proxy-api \
  --atomic \
  --version "$CHART_VERSION" \
  --namespace "$NAMESPACE" \
  --values values.yaml
```

For a local chart checkout, install `application/cli-proxy-api/chart` instead
of the OCI reference.

## Complete xAI OAuth

Start the device flow in the running Pod:

```bash
kubectl exec -it \
  --namespace "$NAMESPACE" \
  deployment/cli-proxy-api \
  -- /usr/local/bin/cli-proxy-api-xai-login
```

The command prints a verification URL and device code. Open the URL, sign in
with the intended xAI account, enter the code, and wait for the terminal to
report that authentication is ready.

Check that an auth file exists without printing its contents:

```bash
kubectl exec \
  --namespace "$NAMESPACE" \
  deployment/cli-proxy-api \
  -- sh -c 'set -- /data/auths/xai-*.json; test -f "$1"'
```

If the Pod or terminal session stops before authorization completes, run the
login command again and use the new device code.

After authentication is stored on the PVC, normal Pod restarts and node moves
do not require another device login. Reauthentication is needed only when the
PVC is lost, the xAI session is revoked, or the refresh token becomes invalid.

## Verify the proxy

Load the CPA API key from the client secret manager, then query the configured
Ingress:

```bash
export CPA_BASE_URL=https://cli-proxy-api.example.internal
export CPA_API_KEY='<load from the client secret manager>'

curl -fsS "$CPA_BASE_URL/v1/models" \
  -H "Authorization: Bearer $CPA_API_KEY" \
  | jq -r '.data[].id'
```

Run a minimal xAI request:

```bash
curl -fsS "$CPA_BASE_URL/v1/responses" \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H 'Content-Type: application/json' \
  --data-binary '{
    "model": "grok-4.5",
    "input": "Reply with exactly: CPA_OAUTH_OK"
  }'
```

## Non-sensitive CLIProxyAPI configuration

Common options are available under `config`:

```yaml
config:
  requestRetry: 3
  maxRetryCredentials: 0
  routing:
    strategy: round-robin
    sessionAffinity: false
  streaming:
    keepaliveSeconds: 15
    bootstrapRetries: 1
```

Use `config.extra` only for additional non-sensitive CLIProxyAPI fields. The
chart rejects fixed deployment fields and API key fields in `config.extra`.
Extend the Secret and renderer contract before adding another sensitive field.

## Storage

The OAuth token store defaults to a chart-managed RWO PVC:

```yaml
persistence:
  auth:
    enabled: true
    storageClass: ""
    existingClaim: ""
    size: 1Gi
```

Set `existingClaim` to reuse a PVC. Disabling persistence uses `emptyDir` and
requires device login after every Pod recreation.

The chart enforces one replica and uses the `Recreate` deployment strategy to
avoid concurrent refresh-token writes.

## Rotate CPA API keys

Update the `api-keys` JSON array in the credentials Secret, then recreate the
Pod so the init container generates a new configuration:

```bash
kubectl rollout restart \
  --namespace "$NAMESPACE" \
  deployment/cli-proxy-api
```

The main container does not mount the source credentials Secret.

## Backup and restore

Back up the auth PVC with the cluster's CSI snapshot or encrypted backup
system. The volume contains OAuth access and refresh tokens and must be handled
at the same sensitivity level as a Secret.

Stop the old Deployment before restoring or attaching a copied token store to
another environment. Do not run two deployments from copies of the same active
refresh-token state.

## Build the runtime image

Build from the repository root:

```bash
podman build \
  --file application/cli-proxy-api/container/Containerfile \
  --tag localhost/cli-proxy-api-runtime:dev \
  .
```

The upstream base image is pinned in the Containerfile. Upgrading that version
requires rerunning the script, chart, OAuth, and restart tests described in
`DESIGN_CN.md`.
