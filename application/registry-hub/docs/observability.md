# Observability

## Health

Both backend listeners expose unauthenticated health endpoints:

| Endpoint | Meaning |
| --- | --- |
| `/healthz` | The process can serve HTTP. Upstream failures do not make this fail. |
| `/readyz` | The backend can accept Registry work: startup completed, storage and internal Distribution checks pass, and the GC gate is open with no recovery barrier or exclusive operation. Shutdown makes this return `503`. |

Storage and Distribution health are rechecked periodically, currently every 60 seconds. Gate transitions update readiness immediately. Readiness is not a per-request guarantee. An unavailable public upstream does not by itself make the backend unready. A closed GC gate can leave reads available while `/readyz` returns `503` because Registry writes and solidification are blocked.

For the default release:

```sh
NAMESPACE=develop
RELEASE=registry-hub
FULLNAME="${RELEASE}-registry-hub"
ADMIN_URL="http://${FULLNAME}-admin.${NAMESPACE}.svc:8081"

curl --fail --silent --show-error "$ADMIN_URL/healthz"
curl --fail --silent --show-error "$ADMIN_URL/readyz"
```

Use a reachable Service DNS address or ClusterIP. An HTTP `200` with an empty body is a successful health response.

## Logs

The backend writes structured JSON logs to stderr. Read them through Kubernetes:

Storage failure records include a bounded operation and error class. When available, `request_id` identifies the Hub request, `operation_id` identifies the solidification task, and `s3_request_id` is the object store's request ID for matching RustFS logs. Credentials, request bodies, and object keys are not logged.

```sh
kubectl -n "$NAMESPACE" logs \
  -l "app.kubernetes.io/instance=${RELEASE},app.kubernetes.io/component=backend" \
  --all-containers=false --tail=100
```

Registry request logs include `request_id`, `operation`, `method`, numeric `status`, `latency_ms`, `cache_source`, and `error_class`. Storage failures also produce warning logs. A warning at startup records disabled client authentication.

Use request IDs to correlate request log entries, and the admin task list to inspect durable work by authority, repository, or digest. Credentials and sensitive upstream responses are excluded from logs and admin output.

## Metrics

The private admin listener exposes Prometheus metrics at `/metrics`. Use an admin credential with `task-admin` permission, such as the temporary netrc file prepared in [Security](security.md#generated-admin-credentials):

```sh
curl --fail --silent --show-error --netrc-file "$ADMIN_NETRC" \
  "$ADMIN_URL/metrics"
```

Configure your monitoring system to scrape this Service with Basic or Bearer authentication from a protected credential source. The chart does not create a ServiceMonitor or install a monitoring stack.

| Metric | What to watch |
| --- | --- |
| `registry_hub_requests_total`, `registry_hub_request_duration_seconds` | Request outcome and latency by operation and cache source. The `status` label uses HTTP reason text, such as `Service Unavailable`. |
| `registry_hub_upstream_requests_total`, `registry_hub_events_total` | Upstream outcomes, retries, and validation events. |
| `registry_hub_tasks`, `registry_hub_tasks_by_kind_state`, `registry_hub_tasks_runnable`, `registry_hub_tasks_waiting` | Durable work by state and kind, plus runnable and waiting totals. |
| `registry_hub_blob_intents`, `registry_hub_blob_intent_limit` | Durable blob intent records by bounded state and the configured active-intent limit. |
| `registry_hub_task_waiting`, `registry_hub_task_retry_attempts`, `registry_hub_task_cancellations`, `registry_hub_solidification_recovery_unknown`, `registry_hub_task_errors` | Bounded wait reasons, retry attempts, cancellation phases, unresolved recovery, and error classes. |
| `registry_hub_storage_warnings_total`, `registry_hub_storage_operation_duration_seconds` | Unexpected storage failures and storage latency. |
| `registry_hub_cache_bytes`, `registry_hub_cache_cleanup_pending_objects` | Temporary cache usage and eligible cleanup backlog. |
| `registry_hub_distribution_api_errors_total`, `registry_hub_distribution_response_validation_failures_total` | Internal Registry errors and invalid responses. |
| `registry_hub_referrer_repair_pending`, `registry_hub_referrer_reconcile_total` | Pending repair and reconciliation outcomes. |
| `registry_hub_gc_gate`, `registry_hub_gc_runs_total`, `registry_hub_gc_blockers` | Write gate state, GC outcomes, and active blockers by bounded reason. |
| `registry_hub_solidification_protection_sessions`, `registry_hub_solidification_distribution_reserved_bytes` | Active protection sessions and reserved unanchored Distribution capacity. |
| `registry_hub_solidification_protected_cache_reserved_bytes`, `registry_hub_solidification_protected_cache_limit_bytes` | Cache input bytes reserved by active blob transfer and the configured cap. |
| `registry_hub_download_partial_reserved_bytes`, `registry_hub_download_partial_sessions`, `registry_hub_download_partial_limit_bytes` | Reserved capacity and active sessions for resumable upstream downloads. |
| `registry_hub_s3_replay_memory_bytes`, `registry_hub_s3_replay_disk_bytes`, `registry_hub_s3_replay_active_sources` | Temporary replay memory, disk, and active bodies used by Backend S3 requests; filesystem storage reports zero. |
| `registry_hub_concurrency_slots`, `registry_hub_rejections_total` | Occupied slots and rate or concurrency rejection. |

Task, blob-intent, gate, partial-download, and replay gauges refresh periodically, currently every 15 seconds, and task and intent gauges aggregate across all pages. Partial-download bytes are quota reservations, not a scan of stored chunk bytes. Cache gauges refresh at startup and hourly. Use Kubernetes or storage-provider metrics for actual PVC, ephemeral-storage, and bucket capacity. The paginated `/api/v1/gc` response reports the current protected cache byte total and reservation count; task records and protection sessions provide per-work details.

Example PromQL for a persistently failed GC gate:

```promql
registry_hub_gc_gate{state=~"failed|failed_unknown"} == 1
```

A reasonable initial alert set covers sustained storage warnings, a failed GC gate, readiness failures, a growing task backlog, and increased `429` or `503` responses. Scope alerts to each instance and allow for expected GC windows. For request metrics, select `status="Too Many Requests"` or `status="Service Unavailable"`.

Metrics use bounded labels. Repository names, tags, digests, tokens, and full URLs are not metric dimensions; use authenticated task queries for object-specific investigation.
