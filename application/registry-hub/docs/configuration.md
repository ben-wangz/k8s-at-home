# Configuration

Use the tested values in `tests/features/<name>/config.yaml` as configuration references. Each entry below explains one feature. All chart parameters and defaults are in [values.yaml](../chart/values.yaml).

## Minimal installation

[Minimal values](../tests/features/minimal/config.yaml) are empty. The chart creates a 20 GiB backend PVC and a 100 GiB Distribution PVC using the default StorageClass. An initialization Job generates and reuses admin credentials, internal Registry credentials, TLS material, and the upload session secret. Client authentication, catalog, Ingress, and GC are disabled by default.

The namespace must exist, the cluster must provide a default StorageClass and enforce NetworkPolicy, and both container images must be reachable. Set the backend image to one you have built and pushed. See the [build and environment instructions](../tests/features/minimal/environment/README.md).

```sh
REPO_ROOT=/absolute/path/to/k8s-at-home
NAMESPACE=develop
RELEASE=registry-hub
BACKEND_IMAGE_REGISTRY=registry.example.com
BACKEND_IMAGE_REPOSITORY=team/registry-hub-backend
BACKEND_TAG=your-built-tag

helm install "$RELEASE" "$REPO_ROOT/application/registry-hub/chart" \
  --namespace "$NAMESPACE" \
  -f "$REPO_ROOT/application/registry-hub/tests/features/minimal/config.yaml" \
  --set-string backend.image.registry="$BACKEND_IMAGE_REGISTRY" \
  --set-string backend.image.repository="$BACKEND_IMAGE_REPOSITORY" \
  --set-string backend.image.tag="$BACKEND_TAG" \
  --wait --wait-for-jobs --timeout 10m

kubectl -n "$NAMESPACE" get jobs,pods,pvc \
  -l "app.kubernetes.io/instance=${RELEASE}"
```

The initialization Job must complete, both Pods must be Ready, and both PVCs must be Bound. Set `global.storageClass` to select one StorageClass for both claims, `global.defaultStorageClass` as a fallback, or each claim's `storageClassName` individually. Use `-` to leave `storageClassName` empty. Service names use the Bitnami common fullname helper; `fullnameOverride` or `nameOverride` changes the generated base name. Use Service DNS or ClusterIP directly when your network can reach it.

## Individual features

Files containing `__RUN_*__` values need real endpoint, bucket, and Secret names before installation. The regression Runner provides those resources only for test runs. Credentials belong in Secrets; their formats are described in [Security](security.md).

| Feature | Tested configuration | Relevant settings |
| --- | --- | --- |
| Client authentication | [client-auth-catalog/config.yaml](../tests/features/client-auth-catalog/config.yaml) | `backend.clientAuth.existingSecret` and `key` select the client credential file. |
| Catalog | [client-auth-catalog/config.yaml](../tests/features/client-auth-catalog/config.yaml) | `backend.catalog.enabled` enables local repository listing. Authenticated users need `catalog` permission. |
| Upstream credentials | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | `backend.extraEnvFrom` or `backend.extraEnvVarsSecret` injects `REGISTRY_HUB_UPSTREAM_AUTH_CONFIG` from a Secret. |
| Upstream transport | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | `REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG` selects authority-specific CA, port, and explicit HTTP rules. `extraVolumes` and `extraVolumeMounts` mount the CA file. |
| Upstream proxy | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | A Secret supplied through `backend.extraEnvFrom` provides `REGISTRY_HUB_PROXY_CONFIG` or standard proxy environment variables. |
| Upstream Blob downloads | [minimal/config.yaml](../tests/features/minimal/config.yaml) | Resumable downloads are enabled by default. Optional transfer, interrupted-transfer salvage, and partial-storage limits use `backend.extraEnvVars`; see [Operations](operations.md#upstream-blob-downloads). |
| Multi-stage solidification | [minimal/config.yaml](../tests/features/minimal/config.yaml) | `backend.solidification` bounds active root demands, blob intents, protection sessions, protected cache inputs, and unanchored Distribution bytes. |
| Backend S3 | [backend-s3/config.yaml](../tests/features/backend-s3/config.yaml) | `storage.backend: s3` stores temporary content and task state in S3. Set the endpoint, bucket, prefix, and credentials Secret. The test uses HTTP and small multipart thresholds. |
| Distribution S3 | [registry-s3/config.yaml](../tests/features/registry-s3/config.yaml) | `registry.storage.type: s3` stores Registry content in S3. Set the bucket, endpoint, credentials Secret, and CA Secret when using a private CA. |
| Storage redirect | [storage-redirect/config.yaml](../tests/features/storage-redirect/config.yaml) | `registry.redirect.enabled` enables Distribution redirects. `allowedEndpoints` must list trusted storage origins; the backend follows them internally. The test's S3 settings supply its storage fixture. |
| Referrers fallback | [referrers-fallback/config.yaml](../tests/features/referrers-fallback/config.yaml) | The test pins Distribution 2.8.3 by digest to exercise automatic fallback when native Referrers is unavailable. Replace its cluster-specific image repository with a reachable reference to the same image. |
| GC | [gc/config.yaml](../tests/features/gc/config.yaml) | `gc.enabled` enables GC. Set `uniqueWriterConfirmed` only after verifying exclusive write access; see [Operations](operations.md#garbage-collection). |

For Backend S3, the chart bounds request replay storage with `storage.s3.replayMemoryBytes` (16 MiB per request), `replayMaxMemoryBytes` (64 MiB total), and `replayMaxDiskBytes` (200 GiB by default). The replay directory is mounted from a Backend-only `emptyDir`; it is temporary request storage, not a persistent image or task store. Size the Pod's ephemeral storage to support the configured disk budget. The backend prepares each PUT body once and reuses its seekable replay source for SDK retries, including multipart part retries.

Solidification defaults are a 24-hour no-progress timeout, 1,024 active root demands, 10,000 blob intents, 1,024 new protection sessions, 200 GiB of protected blob inputs, and 1 TiB of new unanchored Distribution reservations. Bounded pre-upgrade scopes use a separate grandfathered byte ledger during migration and do not increase their digest set. `maxWaitingRoots` bounds active root demand records, including roots currently executing or waiting. `maxProtectedCacheBytes` counts unique cache inputs held for active blob transfer and retry; it does not cap all complete cache objects, partial downloads, or S3 replay files. Waiting work uses no worker, and capacity waiters resume in durable queue-entry order. Configure these limits under `backend.solidification` in [values.yaml](../chart/values.yaml).

The AWS SDK uses its Standard retryer by default: three total API attempts with jittered backoff capped at 20 seconds. Set `AWS_RETRY_MODE` and `AWS_MAX_ATTEMPTS` through `backend.extraEnvVars` or a Secret to use the SDK's supported overrides. The per-request HTTP timeout remains 300 seconds by default; request context deadlines may end an operation sooner.

For client authentication and catalog, use the respective settings from their shared test file. For upstream features, inject only the corresponding configuration into your Secret. The [chart defaults](../chart/values.yaml) also define Ingress/TLS, PVC sizing, image pull Secrets, resources, and scheduling options.

## Extensions

Use `backend.extraEnvVars` and `registry.extraEnvVars` for additional container environment entries. Use the corresponding `extraEnvVarsSecret` setting to load environment variables from a Secret. `backend.extraEnvFrom` also accepts additional Secret or ConfigMap references. Add extra Kubernetes resources with the chart-level `extraDeploy` list.

Generated credentials survive upgrades and uninstall. See [Security](security.md#generated-admin-credentials) for access and [Operations](operations.md#uninstall) for resource removal.
