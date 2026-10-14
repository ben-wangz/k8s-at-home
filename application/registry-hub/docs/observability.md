# Observability

## Health

Both backend listeners expose unauthenticated health endpoints:

| Endpoint | Meaning |
| --- | --- |
| `/healthz` | The process can serve HTTP. Upstream failures do not make this fail. |
| `/readyz` | Startup completed and the backend is ready to serve, with storage and internal Distribution checks passing. Shutdown makes this return `503`. |

Storage and Distribution health are rechecked periodically, currently every 60 seconds. Readiness is not a per-request guarantee. An unavailable public upstream does not by itself make the backend unready. A closed GC gate can block writes while reads and readiness remain available.

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
| `registry_hub_tasks`, `registry_hub_task_errors` | Queued work, terminal failures, and error classes. |
| `registry_hub_storage_warnings_total`, `registry_hub_storage_operation_duration_seconds` | Unexpected storage failures and storage latency. |
| `registry_hub_cache_bytes`, `registry_hub_cache_cleanup_pending_objects` | Temporary cache usage and eligible cleanup backlog. |
| `registry_hub_distribution_api_errors_total`, `registry_hub_distribution_response_validation_failures_total` | Internal Registry errors and invalid responses. |
| `registry_hub_referrer_repair_pending`, `registry_hub_referrer_reconcile_total` | Pending repair and reconciliation outcomes. |
| `registry_hub_gc_gate`, `registry_hub_gc_runs_total` | Write gate state and GC outcomes. |
| `registry_hub_concurrency_slots`, `registry_hub_rejections_total` | Occupied slots and rate or concurrency rejection. |

Task and gate gauges refresh periodically, currently every 15 seconds. Task gauges summarize up to the first 1,000 listed tasks; use the paginated admin API for a complete inventory. Cache gauges refresh at startup and hourly. Use Kubernetes or storage-provider metrics for actual PVC and bucket capacity.

Example PromQL for a persistently failed GC gate:

```promql
registry_hub_gc_gate{state=~"failed|failed_unknown"} == 1
```

A reasonable initial alert set covers sustained storage warnings, a failed GC gate, readiness failures, a growing task backlog, and increased `429` or `503` responses. Scope alerts to each instance and allow for expected GC windows. For request metrics, select `status="Too Many Requests"` or `status="Service Unavailable"`.

Metrics use bounded labels. Repository names, tags, digests, tokens, and full URLs are not metric dimensions; use authenticated task queries for object-specific investigation.
