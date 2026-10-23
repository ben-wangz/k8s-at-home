# Operations

The backend and Distribution each run as a single instance. Use the private admin endpoint and credentials prepared in [Security](security.md#generated-admin-credentials). Examples below assume `ADMIN_URL` and `ADMIN_NETRC` are set. Request and response schemas are available at the authenticated `/docs` and `/openapi.yaml` endpoints.

## Solidification tasks

List tasks, then inspect an individual task:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  "$ADMIN_URL/api/v1/tasks?limit=100"

TASK_ID=replace_with_a_task_id
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  "$ADMIN_URL/api/v1/tasks/$TASK_ID"
```

Follow `nextToken` with the `token` query parameter until it is empty, including when a filtered page has no items. Filters include state, authority, repository, digest, and time. For a mirror path such as `docker.io/library/busybox`, the task authority is `docker.io` and its repository is `library/busybox`.

| State | Operator interpretation |
| --- | --- |
| `pending`, `running` | Blob transfer or root/platform discovery and publication is queued or executing. |
| `retry_wait` | A retry is scheduled; inspect `nextAttemptAt` and `errorClass`. |
| `waiting_cache` | A blob task is waiting for a complete verified cache entry. |
| `waiting_dependencies` | Work is waiting for a root scope, GC admission, or another durable dependency. |
| `waiting_capacity` | Work is waiting for Distribution or protected cache capacity; it uses no worker while waiting. |
| `verified_local` | A blob was verified in Distribution. Its platform or root manifest may still need publication. |
| `solidified` | A root's full original content closure and requested reference were published and verified locally. |
| `failed` | Inspect `errorClass` and `retryable`, then fix the cause. |
| `cancelled` | Work stopped safely. Already-published content remains, and any outstanding publication or Referrers duty remains tracked. |
| `repair_pending` | Referrer or publication repair needs completion. |

After fixing a retryable failure:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/tasks/$TASK_ID/retry"
```

To request cancellation of active work:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/tasks/$TASK_ID/cancel"
```

An accepted request is not proof of completion. Read the task again. Cancellation fences new work; an already-sent publication is retained and protection remains until its remote result is known. Permanent failures cannot be retried through this operation, except `capacity_limit` failures, which start a fresh root or blob round after limits are adjusted. Blob retries require the original verified cache input to remain available and return the new round's task ID. A blob task can be verified while its root remains pending on other layers or platforms.

Solidification proceeds in stages. Blob payloads are uploaded only from complete verified cache entries; background work can discover manifest metadata but does not fetch missing blob payloads. A ready platform manifest can be published by digest independently. Parent indexes and the requested root reference are published after the full original closure is locally available and verified. Cache cleanup follows each blob's durable Distribution verification, subject to active reader and input pins.

The `backend.solidification` settings bound active root demands, blob intents, new protection sessions, cache inputs held by blob workers, and new unanchored Distribution bytes. Bounded pre-upgrade scopes use a separate grandfathered byte ledger and cannot increase that ledger with new content. `maxProtectedCacheBytes` counts unique blob task inputs reserved during transfer and retry; it is not a cap on all complete cache objects or partial downloads. Waiting tasks release worker slots, and quota waiters are resumed in durable queue-entry order. The GC status endpoint reports active sessions and separate new and grandfathered reservation totals; task records expose each task's `protectedCacheBytes`, wait reason, and session references.

If startup recovery keeps the GC barrier active, inspect `gate.recoveryReason`, task `recoveryReason`, and GC session fields `remoteOperationKind`, `remoteDigest`, `remoteSubject`, and `recoveryReason`. Recovery clears an interrupted write only after Distribution readback verifies the expected content or reference. An unavailable read or trusted miss keeps the session and GC barrier active because a delayed request may still commit. `legacy_scope_over_limit` means the persisted graph exceeds the configured closure bound; raise `limits.closureBytes` and restart so recovery can derive a bounded legacy reservation. `legacy_uncheckpointed_write` means the old task has no persisted graph or checkpoints. Cancelling that task is safe only while both remain empty and root publication has not started; recovery clears the blocker after the task reaches a terminal state. Other unresolved legacy scopes remain blocked for operator review. Do not run GC while the barrier remains active.

## Upstream Blob downloads

Blob data is downloaded to temporary persistent chunks and becomes readable only after the full size and digest have been verified. If a connection, transfer attempt, or Backend Pod stops, the next attempt resumes from the last committed byte. Upstreams that ignore Range are fetched again from byte zero. Registry clients and ingress controllers can impose shorter request deadlines than the Backend.

The defaults are a 24-hour total Blob transfer budget, a 300-second per-Range attempt limit, a 60-second wait without upstream data, a 300-second maximum salvage budget for interrupted transfers, a 24-hour partial-download retention period, and a 200 GiB aggregate partial-download limit. Normal EOF persistence continues under the active transfer context and does not use the salvage timeout. Salvage is attempted only while the service is running and a fresh ownership check succeeds. Set these optional Backend environment variables through `backend.extraEnvVars`:

```yaml
backend:
  extraEnvVars:
    - name: REGISTRY_HUB_BLOB_TRANSFER_TIMEOUT_SECONDS
      value: "86400"
    - name: REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS
      value: "300"
    - name: REGISTRY_HUB_UPSTREAM_BODY_IDLE_TIMEOUT_SECONDS
      value: "60"
    - name: REGISTRY_HUB_DOWNLOAD_FINALIZE_TIMEOUT_SECONDS
      value: "300"
    - name: REGISTRY_HUB_DOWNLOAD_PARTIAL_TTL_SECONDS
      value: "86400"
    - name: REGISTRY_HUB_DOWNLOAD_MAX_BYTES
      value: "214748364800"
```

While a Blob is being assembled into the complete cache, storage usage can temporarily approach twice the Blob size. The default backend PVC is 20 GiB while the default per-object limit is 100 GiB; size persistent storage for expected concurrent downloads and cache assembly. A cold Blob response waits for verification before returning its first byte, so a client or ingress with a shorter deadline may need to retry. Retrying reuses committed progress when the upstream supports Range.

For Backend S3, PUT request bodies are first copied to a seekable replay source and validated against the declared length. The default per-source in-memory limit is 16 MiB, the aggregate replay memory limit is 64 MiB, and the replay disk limit defaults to the aggregate download byte limit (200 GiB). The chart mounts the replay directory as a temporary `emptyDir`; it is separate from both persistent storage domains. Allow enough Pod ephemeral storage for concurrent replay sources. S3 API calls use the AWS SDK retryer; the SDK can retry eligible network and service errors up to three total attempts by default, subject to the caller deadline and retry quota. Conditional PUT uncertainty is resolved by checking the operation marker, exact object version, length, and digest before reporting success.

## Cleanup and Referrers

Trigger bounded cleanup of expired upload sessions, eligible temporary objects, orphaned storage objects, and old terminal tasks:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/tasks/cleanup"
```

Cleanup respects retention periods and active object protection. It does not delete published Registry content or replace Distribution GC.

For Referrer repair, query `/api/v1/referrers/reconcile` with the logical `repository` and subject digest. Inspect the current set and pending repair state before choosing an action in `/docs`. `replace` and fallback-to-native `migrate` require the complete intended set and `complete=true`. Migration must verify the native set before it becomes authoritative.

## Garbage collection

GC is disabled by default. The [GC configuration](../tests/features/gc/config.yaml) enables it after explicit confirmation that the backend is the only writer. Check that no other process writes Registry content or its storage. The chart disables Distribution upload purging and grants the backend permissions for its GC Jobs.

Query status and start a run:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  "$ADMIN_URL/api/v1/gc"
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/gc"
```

The gate refuses GC with HTTP `409` while solidification protection sessions or unresolved recovery state remain. A `GC_BLOCKED` error includes `detail.gate`, the first page of `detail.blockers`, and `detail.nextToken` when more sessions exist; use `GET /api/v1/gc?token=...` to page through the status. Task list and detail responses expose `gcProtectionActive` and current `protectionSessions`, including owner counts, pending operations, cancellation phase, and unresolved remote-write state. `quotaWaitAt`, `quotaWaitReason`, and `quotaWaitBytes` report the queue-entry time, resource, and exact Distribution reservation requested by a task waiting for capacity. Task detail pages graph maps and persisted graph nodes with `graphLimit`, `graphToken`, and `graphNodesToken`; owner IDs default to the first task session and use `ownerSessionID`, `ownerLimit`, and `ownerToken` for paging or selecting another session. The GC endpoint also pages owner IDs with `GET /api/v1/gc?ownersFor=<session-id>&ownerLimit=100` and the returned `ownersNextToken`. After admission GC drains accepted writes, reaches `backend_quiesced`, runs GC, restores access, and returns to `open` after success. Push, delete, and upload writes are blocked while the gate is closed. Reads can continue while Distribution remains available. Kubernetes runs official Distribution GC using the same image and storage configuration as the Registry Pod.

Poll status until the run is `succeeded` and the gate is `open`. A `failed` or `failed_unknown` gate remains closed across backend restarts. Inspect the recorded executor identity and corresponding Job before retrying or recovering. Retry starts another GC run; recovery reconciles the previous executor and restores access when safe. Both requests use `{"confirmedNoExecutor":true}` after confirming no executor remains active; Kubernetes additionally validates recorded Job identity and terminal state.

For Kubernetes, use `/api/v1/gc/retry` for a verified failed Job and `/api/v1/gc/recover` for a verified completed Job. Missing Jobs, altered executor configuration, and uncertain completion can cause refusal; preserve the evidence and closed gate until the execution state is established.

Do not reopen the gate by editing persistent state or deleting the Job. [Standalone GC](standalone.md#local-gc) uses an explicitly configured executable instead of Kubernetes Jobs.

## Upgrades

Save the current values and back up persistent resources before changing the image or configuration. Keep the release name, namespace, storage identity, and generated Secret references stable. Use a complete reviewed values file:

```sh
REPO_ROOT=/absolute/path/to/k8s-at-home
NAMESPACE=develop
RELEASE=registry-hub
FULLNAME="${RELEASE}-registry-hub"
VALUES_FILE=/absolute/path/to/reviewed-values.yaml

helm upgrade "$RELEASE" "$REPO_ROOT/application/registry-hub/chart" \
  --namespace "$NAMESPACE" --reset-values -f "$VALUES_FILE" \
  --wait --wait-for-jobs --timeout 10m
```

The values file must include every required customization, including the intended backend image. Check Pod readiness, task recovery, and GC state after the upgrade. An internal TLS or authentication change must update both sides consistently. Changing storage mode or bucket does not migrate existing data.

## Backup and restore

Back up the following as a consistent set, with writers stopped and no active GC executor:

- Distribution content: its PVC or S3 bucket and root directory.
- Backend state: its PVC or S3 prefix, including tasks, cache, checkpoints, leases, deletion generations, and GC state.
- Generated and external Secrets, initialization record ConfigMaps, reviewed Helm values, and image references.

Find generated resources by their instance label:

```sh
kubectl -n "$NAMESPACE" get secrets,configmaps \
  -l "registry-hub.io/instance=${FULLNAME}"
```

Use your storage provider's snapshot or backup tooling and store Secret exports securely. Default generated resources have the `registry-hub.io/managed-by` value `internal-initializer` or `admin-initializer`; external Secrets need their own inventory.

Initialization records bind the generated Secret's UID. Recreating exported Secrets assigns new UIDs, so reapplying old ready records is not a valid restore procedure. For that form of restore, preserve the original secret values in external Secrets and select them through the chart's explicit Secret references described in [Security](security.md#storage-credentials-and-lifecycle). Restore both storage domains before starting workloads, preserve any closed GC gate, and reconcile executor state before admitting writes.

## Uninstall

Finish or stop active work and confirm that no GC executor remains before uninstalling.

```sh
helm uninstall "$RELEASE" --namespace "$NAMESPACE"
```

Helm removes chart-managed resources, including chart-created PVCs. The StorageClass reclaim policy determines what happens to their volumes. Existing claims, external S3 data, and externally managed Secrets are outside the release lifecycle.

Generated Secrets and initialization records survive uninstall because the initialization Job creates them separately. Keep them when reinstalling the same instance. For permanent removal, back up required content, uninstall, then explicitly remove only that instance's retained resources. Regression runs have their own cleanup command in the [test guide](../tests/features/minimal/README.md).
