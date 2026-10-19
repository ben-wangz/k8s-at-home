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
| `pending`, `running` | Work is queued or executing. |
| `retry_wait` | A retry is scheduled; inspect `nextAttemptAt` and `errorClass`. |
| `solidified` | The root and its full content closure were published and verified locally. |
| `failed` | Inspect `errorClass` and `retryable`, then fix the cause. |
| `cancelled` | Work was cancelled before root publication. |
| `repair_pending` | Referrer or publication repair needs completion. |

After fixing a retryable failure:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/tasks/$TASK_ID/retry"
```

To cancel work before root publication:

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  -X POST "$ADMIN_URL/api/v1/tasks/$TASK_ID/cancel"
```

An accepted request is not proof of completion. Read the task again. Cancellation is refused once root publication has started; permanent failures cannot be retried through this operation.

## Upstream Blob downloads

Blob data is downloaded to temporary persistent chunks and becomes readable only after the full size and digest have been verified. If a connection, transfer attempt, or Backend Pod stops, the next attempt resumes from the last committed byte. Upstreams that ignore Range are fetched again from byte zero. Registry clients and ingress controllers can impose shorter request deadlines than the Backend.

The defaults are a 24-hour total Blob transfer budget, a 300-second per-Range attempt limit, a 60-second wait without upstream data, a 24-hour partial-download retention period, and a 200 GiB aggregate partial-download limit. Set these optional Backend environment variables through `backend.extraEnvVars`:

```yaml
backend:
  extraEnvVars:
    - name: REGISTRY_HUB_BLOB_TRANSFER_TIMEOUT_SECONDS
      value: "86400"
    - name: REGISTRY_HUB_REQUEST_TIMEOUT_SECONDS
      value: "300"
    - name: REGISTRY_HUB_UPSTREAM_BODY_IDLE_TIMEOUT_SECONDS
      value: "60"
    - name: REGISTRY_HUB_DOWNLOAD_PARTIAL_TTL_SECONDS
      value: "86400"
    - name: REGISTRY_HUB_DOWNLOAD_MAX_BYTES
      value: "214748364800"
```

While a Blob is being assembled into the complete cache, storage usage can temporarily approach twice the Blob size. The default backend PVC is 20 GiB while the default per-object limit is 100 GiB; size persistent storage for expected concurrent downloads and cache assembly. A cold Blob response waits for verification before returning its first byte, so a client or ingress with a shorter deadline may need to retry. Retrying reuses committed progress when the upstream supports Range.

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

The gate drains accepted writes, reaches `backend_quiesced`, runs GC, restores access, and returns to `open` after success. Push, delete, and upload writes are blocked while the gate is closed. Reads can continue while Distribution remains available. Kubernetes runs official Distribution GC using the same image and storage configuration as the Registry Pod.

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
