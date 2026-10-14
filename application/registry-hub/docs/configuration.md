# Configuration

Use the tested values in `tests/features/<name>/config.yaml` as configuration references. Each entry below explains one feature. All chart parameters and defaults are in [values.yaml](../chart/values.yaml).

## Minimal installation

[Minimal values](../tests/features/minimal/config.yaml) are empty. The chart creates a 20 GiB backend PVC and a 100 GiB Distribution PVC using the default StorageClass. An initialization Job generates and reuses admin credentials, internal Registry credentials, TLS material, and the upload session secret. Client authentication, catalog, Ingress, and GC are disabled by default.

The namespace must exist, the cluster must provide a default StorageClass and enforce NetworkPolicy, and both container images must be reachable. Set the backend image to one you have built and pushed. See the [build and environment instructions](../tests/features/minimal/environment/README.md).

```sh
REPO_ROOT=/absolute/path/to/k8s-at-home
NAMESPACE=develop
RELEASE=registry-hub
BACKEND_REPOSITORY=registry.example.com/team/registry-hub-backend
BACKEND_TAG=your-built-tag

helm install "$RELEASE" "$REPO_ROOT/application/registry-hub/chart" \
  --namespace "$NAMESPACE" \
  -f "$REPO_ROOT/application/registry-hub/tests/features/minimal/config.yaml" \
  --set-string backend.image.repository="$BACKEND_REPOSITORY" \
  --set-string backend.image.tag="$BACKEND_TAG" \
  --wait --wait-for-jobs --timeout 10m

kubectl -n "$NAMESPACE" get jobs,pods,pvc \
  -l "app.kubernetes.io/instance=${RELEASE}"
```

The initialization Job must complete, both Pods must be Ready, and both PVCs must be Bound. By default, the client endpoint is `http://<release>-registry-hub.<namespace>.svc:5000`; the admin endpoint is `http://<release>-registry-hub-admin.<namespace>.svc:8081`. `fullnameOverride` or `nameOverride` changes these names. Use Service DNS or ClusterIP directly when your network can reach it.

## Individual features

Files containing `__RUN_*__` values need real endpoint, bucket, and Secret names before installation. The regression Runner provides those resources only for test runs. Credentials belong in Secrets; their formats are described in [Security](security.md).

| Feature | Tested configuration | Relevant settings |
| --- | --- | --- |
| Client authentication | [client-auth-catalog/config.yaml](../tests/features/client-auth-catalog/config.yaml) | `backend.clientAuth.existingSecret` and `key` select the client credential file. |
| Catalog | [client-auth-catalog/config.yaml](../tests/features/client-auth-catalog/config.yaml) | `backend.catalog.enabled` enables local repository listing. Authenticated users need `catalog` permission. |
| Upstream credentials | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | `backend.extraEnvFrom` injects `REGISTRY_HUB_UPSTREAM_AUTH_CONFIG` from a Secret. |
| Upstream transport | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | `REGISTRY_HUB_UPSTREAM_TRANSPORT_CONFIG` selects authority-specific CA, port, and explicit HTTP rules. `extraVolumes` and `extraVolumeMounts` mount the CA file. |
| Upstream proxy | [upstream-auth-transport/config.yaml](../tests/features/upstream-auth-transport/config.yaml) | A Secret supplied through `backend.extraEnvFrom` provides `REGISTRY_HUB_PROXY_CONFIG` or standard proxy environment variables. |
| Backend S3 | [backend-s3/config.yaml](../tests/features/backend-s3/config.yaml) | `storage.backend: s3` stores temporary content and task state in S3. Set the endpoint, bucket, prefix, and credentials Secret. The test uses HTTP and small multipart thresholds. |
| Distribution S3 | [registry-s3/config.yaml](../tests/features/registry-s3/config.yaml) | `registry.storage.type: s3` stores Registry content in S3. Set the bucket, endpoint, credentials Secret, and CA Secret when using a private CA. |
| Storage redirect | [storage-redirect/config.yaml](../tests/features/storage-redirect/config.yaml) | `registry.redirect.enabled` enables Distribution redirects. `allowedEndpoints` must list trusted storage origins; the backend follows them internally. The test's S3 settings supply its storage fixture. |
| Referrers fallback | [referrers-fallback/config.yaml](../tests/features/referrers-fallback/config.yaml) | The test pins Distribution 2.8.3 by digest to exercise automatic fallback when native Referrers is unavailable. Replace its cluster-specific image repository with a reachable reference to the same image. |
| GC | [gc/config.yaml](../tests/features/gc/config.yaml) | `gc.enabled` enables GC. Set `uniqueWriterConfirmed` only after verifying exclusive write access; see [Operations](operations.md#garbage-collection). |

For client authentication and catalog, use the respective settings from their shared test file. For upstream features, inject only the corresponding configuration into your Secret. The [chart defaults](../chart/values.yaml) also define Ingress/TLS, PVC sizing, image pull Secrets, resources, and scheduling options.

Generated credentials survive upgrades and uninstall. See [Security](security.md#generated-admin-credentials) for access and [Operations](operations.md#uninstall) for resource removal.
