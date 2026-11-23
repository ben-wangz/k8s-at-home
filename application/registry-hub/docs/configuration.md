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

A successful multipart completion received after an SDK retry requires commit confirmation. The backend polls object metadata for up to 60 seconds with backoff from 250 ms to four seconds, bounded by caller cancellation, then verifies operation identity, size, version and the full payload digest once using the enclosing operation's transfer budget. Only a confirmed object can publish cache readiness. An unresolved completion returns an unknown-commit error and retains normal recovery behavior. This exceptional path adds one verification read; ordinary successful completions do not add this readback.

Solidification defaults are a 24-hour no-progress timeout, 1,024 active root demands, 10,000 blob intents, 1,024 new protection sessions, 200 GiB of protected blob inputs, and 1 TiB of new unanchored Distribution reservations. Bounded pre-upgrade scopes use a separate grandfathered byte ledger during migration and do not increase their digest set. `maxWaitingRoots` bounds active root demand records, including roots currently executing or waiting. `maxProtectedCacheBytes` counts unique cache inputs held for active blob transfer and retry; it does not cap all complete cache objects, partial downloads, or S3 replay files. Waiting work uses no worker, and capacity waiters resume in durable queue-entry order. Configure these limits under `backend.solidification` in [values.yaml](../chart/values.yaml).

The AWS SDK uses its Standard retryer by default: three total API attempts with jittered backoff capped at 20 seconds. Set `AWS_RETRY_MODE` and `AWS_MAX_ATTEMPTS` through `backend.extraEnvVars` or a Secret to use the SDK's supported overrides. The per-request HTTP timeout remains 300 seconds by default; request context deadlines may end an operation sooner.

For client authentication and catalog, use the respective settings from their shared test file. For upstream features, inject only the corresponding configuration into your Secret. The [chart defaults](../chart/values.yaml) also define Ingress/TLS, PVC sizing, image pull Secrets, resources, and scheduling options.

## Extensions

Use `backend.extraEnvVars` and `registry.extraEnvVars` for additional container environment entries. Use the corresponding `extraEnvVarsSecret` setting to load environment variables from a Secret. `backend.extraEnvFrom` also accepts additional Secret or ConfigMap references. Add extra Kubernetes resources with the chart-level `extraDeploy` list.

History migration uses three opt-in backend variables. Keep `REGISTRY_HUB_HISTORY_PROJECTION_READS_ENABLED` unset while the receipt history sweep backfills terminal projections. After every backend writer runs the compatible version and the `history_receipts` scan reports a successful round with no pending retries, set it to `true` to use the ordered terminal history projections (48 hours by default). Then set `REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_PREVIEW=true` to run a read-only candidate scan. Inspect `GET /api/v1/workers/receipt_cleanup_preview` and its `scan.counts`, especially `eligible` and `eligible_bytes`, along with the protected and skip counts. Counts describe the current scan round and remain stable when that round resumes. Preview mode does not write completion markers or delete receipts. After reviewing a completed round, disable preview and set `REGISTRY_HUB_FULL_WORK_RECEIPT_CLEANUP_ENABLED=true` to enable conditional deletion. Preview and deletion cannot be enabled together, and both require projection reads.

Retention clocks, diagnostic flush intervals, and event batch budgets are available under `backend.diagnostics` and as backend environment variables. Retention duration values in `GET /api/v1/history/retention` are returned in `durationsSeconds`.

| Helm value | Environment variable | Default |
| --- | --- | ---: |
| `ordinaryEventFlushSeconds` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_FLUSH_SECONDS` | 60 seconds |
| `criticalEventFlushSeconds` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_CRITICAL_FLUSH_SECONDS` | 5 seconds |
| `eventBatchDailyBytesLimit` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_BYTES_LIMIT` | 4 MiB |
| `eventBatchDailyCountLimit` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_COUNT_LIMIT` | 128 batches |
| `eventBatchGlobalBytesLimit` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_BYTES_LIMIT` | 32 MiB |
| `eventBatchGlobalCountLimit` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_COUNT_LIMIT` | 1,024 batches |
| `eventBatchDailyCriticalReserveBytes` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_BYTES` | 1 MiB |
| `eventBatchDailyCriticalReserveCount` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_DAILY_CRITICAL_RESERVE_COUNT` | 32 batches |
| `eventBatchGlobalCriticalReserveBytes` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_BYTES` | 8 MiB |
| `eventBatchGlobalCriticalReserveCount` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BATCH_GLOBAL_CRITICAL_RESERVE_COUNT` | 256 batches |
| `eventRetentionSeconds` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_RETENTION_SECONDS` | 7 days |
| `eventBudgetRetentionSeconds` | `REGISTRY_HUB_DIAGNOSTIC_EVENT_BUDGET_RETENTION_SECONDS` | 8 days |
| `collectionRetentionSeconds` | `REGISTRY_HUB_DIAGNOSTIC_COLLECTION_RETENTION_SECONDS` | 30 days |
| `retryRequestRetentionSeconds` | `REGISTRY_HUB_RETRY_REQUEST_RETENTION_SECONDS` | 30 days |
| `transferRetentionSeconds` | `REGISTRY_HUB_DIAGNOSTIC_TRANSFER_RETENTION_SECONDS` | 30 days |
| `transferExpiryRetentionSeconds` | `REGISTRY_HUB_TRANSFER_EXPIRY_RETENTION_SECONDS` | 30 days |
| `terminalHistoryRetentionSeconds` | `REGISTRY_HUB_TERMINAL_HISTORY_RETENTION_SECONDS` | 48 hours |
| `fullWorkReceiptRetentionSeconds` | `REGISTRY_HUB_FULL_WORK_RECEIPT_RETENTION_SECONDS` | 3 days |
| `historyCleanupPreviewTTLSeconds` | `REGISTRY_HUB_HISTORY_CLEANUP_PREVIEW_TTL_SECONDS` | 10 minutes |
| `historyCleanupResultRetentionSeconds` | `REGISTRY_HUB_HISTORY_CLEANUP_RESULT_RETENTION_SECONDS` | 7 days |
| `historySweepIntervalSeconds` | `REGISTRY_HUB_HISTORY_SWEEP_INTERVAL_SECONDS` | 15 seconds |
| `historySweepMaxIntervalSeconds` | `REGISTRY_HUB_HISTORY_SWEEP_MAX_INTERVAL_SECONDS` | 300 seconds |
| `retentionSweepIntervalSeconds` | `REGISTRY_HUB_HISTORY_RETENTION_SWEEP_INTERVAL_SECONDS` | 15 seconds |

Retention durations accept 1 second through 365 days. Event-budget retention must be at least event retention; collection-receipt retention must be at least both event and full-receipt retention; full-receipt, transfer-observation and transfer-expiry retention must be at least terminal-history retention. Ordinary flush accepts 1 through 3,600 seconds, critical flush 1 through 60 seconds, and history and retention sweep intervals accept 1 through 900 seconds. The history sweep maximum must be at least its base interval. `historySweepIntervalSeconds` controls source history projections and receipts, `historySweepMaxIntervalSeconds` caps exponential idle-scan backoff, and `retentionSweepIntervalSeconds` controls expiration cleanup. `backend.inventory.snapshotRetentionSeconds` remains the separate control for physical inventory snapshots.

Batch limits and critical reserves must be positive, and each reserve must fit within its corresponding daily or global total. The same configured limits are used by both event writers and the batch-budget summary in `GET /api/v1/events`.

Generated credentials survive upgrades and uninstall. See [Security](security.md#generated-admin-credentials) for access and [Operations](operations.md#uninstall) for resource removal.
