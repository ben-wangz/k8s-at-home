# Management diagnostics and storage inventory

`registry_hub_storage_work_operations_total` and `registry_hub_storage_work_duration_seconds` attribute storage calls to bounded `kind`, `purpose`, `operation`, `key_class`, `reason`, and `result` labels. `registry_hub_s3_http_attempts_total` and `registry_hub_s3_http_attempt_duration_seconds` count individual S3 HTTP attempts, including SDK retries, by bounded `domain`, `kind`, `purpose`, `operation`, `key_class`, `reason`, and `result` labels. `registry_hub_registry_http_attempts_total` and `registry_hub_registry_http_attempt_duration_seconds` count internal Distribution HTTP requests at the Go `RoundTripper` boundary by bounded `kind`, `purpose`, method, route class, reason, and result; redirects and bearer challenge retries are separate observations. Route classes never contain repository, tag, or digest values. Domains are `state` and `registry`; key classes and reasons are fixed categories and never contain an object key or digest. Purposes distinguish ordinary work, queue polling, checkpoints, and lease operations. Requests and service work without a worker context have separate categories; unknown worker identities collapse to `other`. Store metrics measure calls at the storage interface, while HTTP attempt metrics measure requests observed at their respective transport boundaries; none represents wall-clock task latency.

For a fixed observation window, use `increase()` to compare observed calls with configured scan and checkpoint budgets. These queries report measured counts after the new binary has run for the selected window; they cannot provide pre-deployment values.

```promql
sum by (kind, purpose, operation, key_class, reason, result) (
  increase(registry_hub_storage_work_operations_total[1h])
)
```

```promql
sum by (domain, kind, purpose, operation, key_class, reason, result) (
  increase(registry_hub_s3_http_attempts_total[1h])
)
```

```promql
sum by (kind, purpose, operation, reason, result) (
  increase(registry_hub_registry_http_attempts_total{method="HEAD"}[1h])
)
```

`registry_hub_background_scan_rounds_total{kind,result}` counts ended source, statistics, and maintenance scan rounds plus inventory snapshot publication rounds. `complete` means the round completed without an unresolved scan error; `incomplete` means traversal ended with work/errors still requiring a later retry. Use the counter for scan counts, including across process restarts:

```promql
sum by (kind, result) (
  increase(registry_hub_background_scan_rounds_total[1h])
)
```

`/api/v1/workers` exposes the latest durable round status, configured cadence, checkpoint budget, and lease state, but not a cumulative round count. Source sweeps cap their event-free interval at one minute; statistics and history sweeps cap at two and five minutes. Full inventory source reconciliation runs five minutes after publication, or at the configured refresh interval when longer. Empty queue polling is capped at five seconds; physical storage collection starts promptly and repeats after 15 minutes. These are schedules, not throughput guarantees: scan duration, admission, storage latency, restarts and lease contention add time. Current post-fix observed values require a deployment and must be reported separately from the 0.1.25 baseline in the analysis report.

Source, statistics, history and maintenance scan status is read from its durable checkpoint. The legacy status summary is refreshed on lease, success, error, or dependency-wait transitions, otherwise no more often than every 30 seconds; it is used only when no checkpoint exists.

Durable queues use coalesced local wake hints and an idle scan backoff capped at five seconds; admission and storage time add to that interval. A local durable enqueue also promotes an idle queue already waiting for admission. Source sweeps, statistics and history sweeps double their event-free delay from 15 seconds, with caps of one, two and five minutes respectively. Writes to matching source prefixes send coalesced local wake hints. A hint advances an idle deadline to at most five seconds away without postponing an earlier deadline or bypassing an explicit retry bound; a hint received during a scan remains buffered for the following pass. Missed hints are corrected by periodic full scans, so the correction pass is scheduled no later than the cap after the preceding completed pass, plus scan duration and admission/storage delay. Other replicas and restarts use the same durable checkpoints and polling fallback. Empty queue checkpoints publish at most once per 30 seconds. Ordinary admission remains limited to two concurrent units, with active queues prioritized and maintenance admitted after at most three consecutive priority admissions (or alternating with aged maintenance). Running work is never preempted, so this policy bounds admission turns rather than promising a wall-clock deadline when storage is slow. The physical storage collector retains its separate capacity.

Cache-ready activation events receive active-work priority. A consumed event is retained across admission timeouts and protection recovery pauses until it can execute or the service stops. Periodic durable reconciliation still recovers process restarts and missed notifications. Execution projections advance past demand or graph entries removed after listing; storage failures other than a confirmed missing object retain normal retry behavior.

All routes below use the private admin listener. Grant `observe` for reads and metrics, and `work-retry` for conditional background retries. Existing `task-admin` credentials include both permissions. Authentication and GC/write authorization remain independent. GET requests read bounded metadata; they do not fetch missing payloads, create protection sessions, repair indexes, or clean storage.

## Investigation routes

| Route | Evidence |
| --- | --- |
| `GET /api/v1/status` | Version, process instance, readiness, independently observed inventory, queues, statistics, probes and workers. |
| `GET /api/v1/images/status` | Durable build checkpoint, source scan, image/graph phase and background observations. Available before the first inventory publication. |
| `GET /api/v1/storage/status` | Durable physical collector phase, committed domain lower bounds, scheduling, retained failures and previous report freshness. Available before the first report and after its expiry. |
| `GET /api/v1/workers` and `/{workerID}` | Admission/execution/checkpoint state, timing budgets, heartbeat, scan success and entity progress. Solidification includes configured concurrency and active task IDs. |
| `GET /api/v1/images/{imageID}/projection` | Observed source revision, projected revision and revision captured by a retained image snapshot. |
| `GET /api/v1/images/{imageID}/execution-facts` | Paginated durable execution sources, independent of published snapshots and captured task lists. Optional `digest` and `generation` filters select a manifest generation. |
| `GET /api/v1/images/{imageID}/dependencies` | Captured transitive dependencies, parent digests, missing content, intent/task links and a link to direct task edges. |
| `GET /api/v1/tasks/{taskID}/dependencies` | Direct persisted graph edges, cache metadata and generation/GC-epoch-qualified local verification evidence. |
| `GET /api/v1/blob-intents` and `/{intentID}` | Demand without a task, activation state, retries, errors and a bounded demand page. |
| `GET /api/v1/blob-intents/{intentID}/notifications` | Indexed notification variants, excluded roots, current revision matches and work checkpoints. |
| `GET /api/v1/blob-intents/{intentID}/notifications/{workID}/targets` | Paginated owner/protection candidates, exclusions and wake work confirmation. |
| `GET /api/v1/background` and `/{kind}` | Active jobs and retained completion receipts; exact filters, opaque revisions and associated resources. |
| `GET /api/v1/background/{kind}/{jobID}` | Checkpoint phase, source revision, retry/error history, recovery time and event links. |
| `GET /api/v1/events` | Durable state transitions and bounded attempt, storage failure and recovery observations. |
| `GET /api/v1/transfers` and `/{transferID}` | Upstream download, client upload and Distribution write observations. Transfer completion, cache commit, residual data and quota release are separate fields. |
| `GET /api/v1/download-reservations` | Quota reservations including those with absent or superseded download state. Links to cleanup work when known. |

Task queries additionally accept `kind`, `waitReason`, `errorClass`, `retryable`, `imageID` and `blobDigest`. Background lists accept `kind`, `state`, `reason`, `authority`, `repository`, `imageID`, `taskID`, `intentID`, `transferID`, `digest`, `errorClass` and `retryable`. States include `pending`, `running`, `waiting_dependency`, `retry_wait`, `failed`, `done` and `superseded`. A `state=done` or `state=superseded` query enumerates receipts directly. Other diagnostic filters and response schemas are in `/openapi.json`.

Diagnostic pages inspect at most 32 source records. Filtering can produce an empty page with a continuation; continue until `nextToken` is absent. Diagnostic continuations are encrypted and bind the route, filters and page size, with a 30-minute lifetime. Image dependency pages use the inventory cursor contract and a maximum of 32 objects. Live job/intent/event enumeration can observe concurrent changes; deduplicate jobs by kind, ID and revision. Snapshot pages remain immutable. Invalid or changed filters return 400; expired cursors or explicitly requested expired snapshots return 410. Latest inventory/storage observations that have not been collected or have expired return 503.

## Interpreting progress

Admission waits have a separate 15-second bound. Execution starts after admission, using a fresh 10- or 15-second budget. Checkpoint commits have a separate five-second bound with the original lifecycle cancellation and lease/CAS guards. Earlier caller deadlines still apply. Statistics release shared capacity after at most four entities or a one-second quantum between entities. Genuine slow storage remains a failure and uses the existing retry backoff.

These shared budgets apply to admitted queues, source scans, statistics and ordinary maintenance, with two ordinary admission slots. Physical storage inventory has a separate single collector slot and 250 ms pacing, including leader coordination. Worker fields `admissionPool` and `admissionCapacity` describe the actual pool; `sharedConcurrency` is zero for the dedicated collector. Independent probes, event collection and inventory construction report their own budgets without claiming shared concurrency. Inventory construction uses its configured batch timeout and commits within that batch.

Startup and runtime protection recovery use the same durable state machine. Each execution quantum has a 60-second budget, individual operations have at most 30 seconds, and checkpoint/lease calls have a separate 15-second coordination budget. Small completed steps are checkpointed in batches of at most eight; phase transitions and cooperative yields are committed immediately. A crash may replay the last incomplete batch through conditional writes and readback. Cooperative yields do not replace failure history with timeout errors. Genuine storage failures use worker backoff from two to 64 seconds, independently of SDK retries. A reserved 90-second lease renews every 30 seconds; losing it cancels execution.

`GET /api/v1/workers/protection_recovery` includes persisted `recovery` progress: generation, owner/fence, gate epoch/state, phase/substep, current logical entity, cursor counts and offsets, reference half, verified remote bytes, yield count, checkpoint time and next retry. Raw storage keys, continuations, hash state and reservation maps are omitted. Reading this metadata never advances recovery. `index_repair` describes the representation-index phase within this worker. Health and admin listeners remain available during recovery; readiness remains false until the durable completion is confirmed and the gate is open.

The gate's `recoveryTrigger` and recovery worker's `trigger` preserve the original kind, reason, task/work/attempt IDs, time and sanitized error across claims and restarts. `recoveryReason=startup_reconcile` describes the internal reconciliation mode; inspect the trigger for the initiating event. Trigger metadata is independent of the worker's current failure history. Older barriers without trigger evidence report `kind=unknown` rather than an inferred startup cause.

Ordinary terminal cleanup is durable `terminal_cleanup` work. Query `/api/v1/background/terminal_cleanup?taskID=<taskID>` and inspect `terminalCleanup`: cleanup/round identity, generation, phase/substep, processed steps, traversal positions, yields, confirmation and associated recovery ID. `processed` counts state-machine steps; demand/session cursor positions describe their current traversal rather than cumulative unique objects. Running jobs also show the remaining execution budget after their first checkpoint; the job's `executionBudgetSeconds` records the admitted budget. The task's `terminalCleanupID` is the durable handoff identity; reconciliation repairs a missed enqueue. Enqueueing does not release protection. Each 15-second execution quantum handles at most 16 small steps and checkpoints each completed step. Demand, shared-demand, session and quota scans retain their cursors. A journal precedes each protected metadata mutation; a started conditional write may finish across the quantum boundary within a separate five-second lifecycle-bound budget. Confirmation and checkpoint calls also have finite coordination budgets and validate the work claim, task lease, cleanup identity and gate epoch. Journal keys and content hashes are omitted from management responses.

Completed cleanup receipts retain an optional `finalSummary` (schema version 1, at most 8 KiB) with identity, phase/outcome, processed steps, yields, confirmation/recovery association and execution/completion times. Outcomes distinguish ordinary completion, superseded work and recovery reconciliation. The summary excludes payload bodies, keys, journals and cursors. List and detail endpoints use the same summary. Missing or unsupported legacy evidence reports null counters with `coverage=unavailable`; a surviving matching active checkpoint can upgrade a legacy receipt. Completed traversal totals remain unknown. Archive retries preserve an existing valid summary.

Quota wake work is visible under `/api/v1/background/quota_wake` and `/api/v1/workers/quota_wake`. Task `quotaWait` reports original and executable wait ages independently, with current coverage and a responsible session, predecessor or recovery worker when known. A recent heartbeat or an incomplete scan does not establish eligibility. Recovery dependency checks have their own timestamps and do not count as execution progress or storage failures.

Budget exhaustion before issuing a mutation yields without a storage retry or global recovery. A confirmed write whose journal checkpoint fails retains its journal for retry. A genuinely unresolved write or remote result keeps protection and enters recovery; its trigger links back to the cleanup work. Work retries use the existing queue backoff. Storage probe failures retain both the primary stage error and auxiliary cleanup error; auxiliary cleanup has its own five-second bound and stops when the service lifecycle is cancelled.

The physical collector honors the durable five-minute next-scan schedule before admission, inner lease acquisition or checkpoint polling. Its quantum performs at most eight transitions and targets eight seconds within the 15-second execution budget. Source pages grow from four to at most 32 entries after three fast pages and shrink after timeout or processing pressure. Prepared output pages contain at most 32 objects or 1 MiB, and new checkpoints are bounded to 4 MiB. Exact prepared rows are committed before immutable output writes; replay retains their timestamps and deterministic usage runs. Legacy pending batches are adopted before new limits apply. Metadata reuse is bounded to a quantum and never authorizes deletion.

Storage status reads at most the collection checkpoint, current report and maintenance checkpoint, plus process-local worker observations. It exposes no raw continuations, physical keys, pending rows or usage maps. `objectsCommitted` and per-domain bytes are lower bounds from durable output; `entriesExamined` includes listed entries excluded from output. Execution units are quanta, not objects. Unknown totals and percentage remain null. Domain state is `waiting` until that domain is reached. The previous report remains independently describable after expiry; summary, usage and object APIs still require a valid completed report. Source read failures are disclosed separately. This endpoint accepts no query parameters.

Blob dependency diagnostics inspect verified cache representations by content identity, preferring the current intent media type. Manifest dependencies retain exact representation semantics. `cacheVersion` describes current verified cache evidence and `intentCacheVersion` describes captured work; an older intent version does not reject a coherent newer payload. Size conflicts, corrupt metadata and unresolved version replacement are unknown. `knownMissingBytesInPage` counts only definite missing blocking blobs once per digest within that page. Distribution verification can establish availability after cache cleanup.

When a blob releases protected cache and its `verified_local` state is persisted, it rechecks the oldest protected-cache waiter. FIFO admission and capacity checks still apply. This prevents waiters from depending on a complete periodic reconciliation sweep after the releasing task leaves the queue.

Background leader acquisition also requires admission and has a five-second execution bound. Renewal and release use a reserved coordination budget of at most five seconds, independently of bulk capacity; renewal also stays within one quarter of the requested lease TTL. They retain the requested TTL and report `lease_renewal` or `lease_release` timeout scope when applicable. Failed renewal cancels leadership. Nested leases acquired within admitted work use the same finite coordination bounds. An execution timeout can still release its lease within the original lifecycle budget; shutdown starts no detached release requests and leaves held leases to expire. Initial scan-checkpoint reads have a separate five-second bound, and inventory builder initialization has an admitted 15-second bound.

Worker heartbeat, successful scan completion and entity progress have different meanings. Successful empty scans refresh `lastSuccessAt` and `lastScanCompletedAt` without inventing entity progress. A heartbeat alone does not prove completion. Scan summaries expose `ageSeconds` since the last successful full round, `scheduleOverdue` after the next planned scan plus 90 seconds, and `failed` for an unrecovered error newer than the last success. Data becomes stale after the maximum normal interval plus 90 seconds: one minute for ordinary source scans, two minutes for statistics, five minutes for history, and 15 minutes for physical storage inventory. A future retry cannot renew old evidence. Probe and queue heartbeat policies remain independent. Inventory status retains the repository reader's freshness based on `lastSourceCheckAt`, falling back to the original observation time; expiry takes precedence. Missing observations and read errors are explicit. A failed build checkpoint read makes build state unknown; an expired snapshot does not imply a build is running.

Projection reports distinguish `projection_pending`, `snapshot_pending`, `captured_as_observed` and `unknown`. Their coverage is a set of non-atomic source sweeps. Zero jobs cannot establish that every source commit has been observed, and matching revisions do not prove global catch-up. Source revisions are opaque identities, with no lexical ordering. The current lifecycle and a retained snapshot can legitimately disagree during projection delay.

The image projection response reads up to four independently indexed execution sources and four legacy captured root task IDs at the captured manifest generation. Follow `execution.factsURL` to enumerate the indexed scope. Root execution links are saved before execution. Shared blob links derive from durable demands and generation-qualified root/platform graph nodes; their start time is the later of the original blob execution and demand creation. They preserve the original blob task ID and separately identify the root task. Legacy or unreconciled associations remain unknown. `execution.knownFirstStartedAt` preserves the earliest time in the observed facts and captured lifecycle, while `capturedFirstStartedAt` belongs to the immutable snapshot. Missing indexed history does not prove execution never started.

Source reconciliation and history sweeps persist their cursor, buffered page, per-entity backoff and successful-round status under a renewed lease. Queue enumeration persists its partial summary and cursor together. Statistics persist their cursor, accumulators, invalid-round marker and scheduling backoff; after restart they restore the last complete metrics before resuming the partial round. Partial aggregates never become complete totals. Quota and filesystem orphan maintenance also retain their continuation and backoff. Checkpoints use CAS and fencing, and permit replay since the last checkpoint. Failed checkpoint writes relinquish leadership and reload durable state; an old lease owner cannot replace a newer checkpoint. Worker output identifies the lease owner, fence and `durable_checkpoint` scope. Index repair remains a startup operation, and periodic index maintenance explicitly reports `not_implemented`.

Source, statistics and maintenance checkpoints retain `lastError`, `recoveredAt` and bounded diagnostic outboxes. Recovery preserves the earlier failure and emits a separate event; unrelated entities and attempts are not coalesced. Protection recovery retains the same failure/recovery history in a separate record, using a finite checkpoint budget from the original lifecycle when available. Lease coordination and process observations use the best-effort event path. Store failure, interruption before checkpoint and outbox retention can still leave declared history gaps.

Dependency responses link the current intent, activation worker and observed notification work, including its checkpoint and next retry. Config and layer objects both receive live intent links. Task links require an existing task. Complete cache with a `waiting_cache` intent reports `activation_pending`. Notification coverage identifies the current revision without an excluded root; follow `notificationsURL` for indexed variants, including excluded-root work. Enqueue and history reconciliation maintain that index; missing legacy entries do not establish global completeness. Target pages traverse owner and cache-protection references and show actual wake work or `not_enqueued`/`excluded`/`unknown`. The same root can appear in both sources; `confirmedTargets` counts reference confirmations, not distinct roots.

Image snapshots retain parent digests from manifest metadata independently of task graph retention. `parentEdgesScope` distinguishes captured, partial and legacy unknown evidence. A shared object retains up to 32 parent digests and sets `parentEdgesTruncated` when more exist. Captured parent links do not silently change when live task graphs are cleaned; live intent and notification observations remain separately identified.

Build status reports source-scan start, current build start and elapsed seconds, the latest publication and complete publication times, previous build/refresh durations, and seconds until the current snapshot expires. A negative expiry interval preserves the actual expiry window. `localDirtyHint` only describes this process; it is not a complete source backlog. Source and graph checkpoint token fields contain opaque hashes for progress comparison.

Protection recovery reports `durable_phase_and_substep` resumability. Its generation and nested checkpoints survive process restarts; diagnostic reads expose that evidence without advancing recovery or bypassing an unresolved barrier.

## Client storage failure logs

Each registry client request writes one existing `registry request` record. Storage failures add bounded `failure_stage`, `failure_cause`, and `failure_elapsed_ms` fields to that record; they do not emit per-chunk or per-retry entries. Stages identify the failed lookup, lease, upstream fill, representation commit, or cache verification step. Expected upstream client responses such as 401 and 404 do not add failure fields or WARN severity. The log omits raw errors, object keys, payload digests, and credentials. Use `request_id` for correlation. `failure_elapsed_ms` measures the enclosing lookup, intent write or fill call; an inner stage reported from that call does not have its own timer. `latency_ms` measures the full request.

## Solidification performance logs

The default JSON logger emits sparse INFO summaries and retains the existing WARN failure entry. Routine dependency waits, queue polling and progress heartbeats emit no additional performance logs. Observations use existing execution data and introduce no storage reads, writes or payload verification.

| Message | Meaning and measurements |
| --- | --- |
| `solidification blob verified` | A blob is verified in Distribution. `source=cache` indicates an upload; `source=distribution` indicates existing content reused after verification or a current generation/GC-epoch-qualified observation. `size_bytes` is the declared object size. This event does not imply manifest anchoring or safe cache removal. |
| `solidification manifest anchored` | A protection session newly transitioned to anchored. Includes `digest`, `media_type`, `scope_bytes`, `blob_count` and `anchor_age_seconds` since that session was created. A selected platform can anchor while an index waits for other platforms. Reconciliation of an already anchored session does not repeat this log. |
| `solidification root completed` | Root processing returned successfully, including required manifest/tag publication. Includes closure manifest/blob counts and bytes, discovery duration and whether discovery was reused. This precedes the task manager's terminal state commit and subsequent cleanup. |
| `solidification failed` | An execution attempt failed. Its existing error class/stage now accompanies elapsed time and observed upload/readback bytes, including costs incurred before failure. |

All summaries identify the task, round, attempt, authority, repository, content generation and retry count. `task_age_seconds` starts at task creation and includes earlier dependency waits and retries; it is not active execution time or time since client download completion. `execution_seconds` covers only the current processor invocation through the logged milestone. Root completion reports the final invocation, not cumulative execution across earlier yields. `anchor_age_seconds` starts at protection admission, which can occur later than the initial client demand.

Blob timings separate `cache_open_seconds` (strict cache open/verification), `upload_seconds` (Distribution PUT request through response headers), and `distribution_verify_seconds` (Distribution verification/observation checks). The total also includes leases, admission, metadata, upload-session creation and checkpoint work. Timings can overlap nested work and must not be summed across blob and root events as image latency. `uploaded_bytes` counts payload bytes consumed by the PUT transport, including failed attempts, rather than acknowledged bytes. `distribution_readback_bytes` and `distribution_readback_seconds` measure full digest readback body consumption/hashing; reused verification evidence contributes zero. Root and blob checks share an object lease and exact durable observation for ordinary blob verification. An independent root verification runs as queued payload work, so its readback appears in payload metrics rather than the root attempt's log. Failed or invalidated attempts and independent manifest or tag checks can still add readbacks.

For a time window, compare verified blob count and declared bytes, upload bytes per wall-clock second, upload bytes per PUT second, reuse among executed blob attempts, p50/p95 blob execution time, selected-platform anchor age, and failure attempts. These distinguish transfer speed, reuse and waiting. Shared layers and manifest scopes overlap: summing `scope_bytes` or root closure sizes does not measure network traffic. A blob verification followed by a later metadata failure can emit both verification and failure for one attempt; count its byte observations once using `(task_id, round_id, attempt_id)`. Transport retries that failed before verification remain visible in WARN entries. Cancellation, crashes and dropped logs can leave gaps, so these logs are operational observations rather than an exact accounting ledger. Use durable task/transfer diagnostics for in-flight or incomplete work and the payload metrics below for continuously collected I/O totals.

For example, aggregate JSON lines retained from all replicas for a UTC window with `jq`. The result counts successful blob observations, distinct root rounds, failure attempts, and uploaded MiB (including failed blob attempts), deduplicating repeated ingestion and multiple observations of one attempt:

```sh
jq -s --arg start '2026-11-06T00:00:00Z' --arg end '2026-11-06T01:00:00Z' '
  [.[] | select((.time | sub("\\.[0-9]+Z$"; "Z")) >= $start
    and (.time | sub("\\.[0-9]+Z$"; "Z")) < $end)] as $window |
  [$window[] | select(.msg == "solidification blob verified")]
    | unique_by([.task_id, .round_id, .attempt_id]) as $verified |
  [$window[] | select(.task_kind == "blob" and
    (.msg == "solidification blob verified" or .msg == "solidification failed"))]
    | group_by([.task_id, .round_id, .attempt_id])
    | map(max_by(.execution_seconds)) as $attempts |
  {verified_blobs: ($verified | length),
   reused_blobs: ([$verified[] | select(.source == "distribution")] | length),
   completed_roots: ([$window[] | select(.msg == "solidification root completed")]
     | unique_by([.task_id, .round_id]) | length),
   failure_attempts: ([$window[] | select(.msg == "solidification failed")]
     | unique_by([.task_id, .round_id, .attempt_id]) | length),
   uploaded_mib: (([$attempts[].uploaded_bytes] | add // 0) / 1048576),
   put_seconds: ([$attempts[].upload_seconds] | add // 0)}
' solidification.jsonl
```

Byte totals are assigned to the observation's timestamp; an attempt spanning the window boundary contributes when its summary is logged. Production task execution populates attempt IDs; older logs without them cannot distinguish retries reliably.

## Refresh performance acceptance

Payload work is exposed through `registry_hub_payload_bytes_total{kind,purpose,operation}` and `registry_hub_payload_duration_seconds` with bounded labels. Purposes distinguish cache verification, client delivery, Distribution upload/readback, download assembly, and cleanup verification. `read`, `write`, and `hash` count logical object/body work; filesystem version hashing is included. Metadata also contributes to these logical counters. `s3_read` and `s3_write` measure consumed SDK HTTP bodies, including retry attempts and error responses. They overlap logical storage work: do not add them to `read`/`write` as though they were independent copies. They exclude protocol headers and server-side physical disk activity.

Use this query for consumed S3 request/response body bytes. These bytes include retries and error responses and exclude protocol headers and server-side physical disk activity:

```promql
sum by (kind, purpose, operation) (
  increase(registry_hub_payload_bytes_total{operation=~"s3_read|s3_write"}[1h])
)
```

Inventory snapshot objects have dedicated counters. `registry_hub_inventory_snapshot_object_writes_total` counts successful Store PUTs that create immutable objects below `inventory/v1/snapshots/`; `registry_hub_inventory_snapshot_object_write_bytes_total` counts the bytes read from those successful PUT bodies. These exclude failed attempts and objects reused after an already-exists or unknown-commit response. The `inventory_snapshot` storage key class also makes all PUT attempts visible by result:

```promql
sum by (kind, purpose, reason) (
  increase(registry_hub_inventory_snapshot_object_writes_total[1h])
)
```

```promql
sum by (kind, purpose, reason) (
  increase(registry_hub_inventory_snapshot_object_write_bytes_total[1h])
)
```

```promql
sum by (kind, purpose, reason, result) (
  increase(registry_hub_storage_work_operations_total{operation="put",key_class="inventory_snapshot"}[1h])
)
```

`registry_hub_distribution_verification_total{result}` and `registry_hub_distribution_verification_seconds{result}` distinguish successful observation reuse, full readback, deferred work, and errors. The `wait` duration covers the handoff only; queue and admission durations are reported separately. Dispatch runtime status exposes candidate pages and reads separately from canonical repair reads, so index rebuild work is not mistaken for warm dispatch cost.

Counters aggregate at EOF, errors, and Close, including partial consumption; opening and closing an unread body contributes zero consumed bytes. Duration histograms measure active I/O or verification time. Nested and concurrent durations overlap and cannot be summed into request latency. For a complete filesystem layer of size B, strict cache verification reads B and verification plus consumption reads 2B; strict S3 verification plus consumption remains 2B before retries. Metadata-only internal readiness consumes zero payload bytes. The initial Distribution readback still hashes the full object.

Compare cache commit, activation, task execution, upload, readback, and platform anchor timestamps separately. Discovery snapshots reduce repeated immutable edge writes and dependency notifications can target affected ancestors, while compensation scans still have a distinct cost. Generated 64/256/1024 MiB local fixtures can be measured with `REGISTRY_HUB_LARGE_LAYER_TEST=1 go -C application/registry-hub/src test ./internal/artifact -run '^TestLargeLayerPayloadAmplification$' -count=1 -timeout 30m -v`. The paired historical read path and optimized path share one filesystem and deterministic payload. These are warm local reads; they do not simulate a cold upstream transfer or establish cluster throughput. Run comparisons serially and retain the reported byte bounds alongside timings.

`TestInventoryRefreshLatencyWithStableContentAndChangingReference` defines a local regression workload of four completed roots and four distinct manifests/configs, their tag and digest references, and a reference updated every 25 checkpoint steps. It uses durable filesystem storage and loopback Distribution, with one millisecond of additional latency per graph edge read. The targets are a complete first publication within 60 seconds and a complete subsequent refresh within 30 seconds, with no edge rereads for unchanged terminal graphs. Every step reloads the persisted checkpoint, including during continued reference writes.

Terminal projection reuse requires the exact task storage revision, matching GC epoch and an open gate without recovery or cleanup. Changes invalidate reuse. Cached manifest topology reuse requires the same content generation and a completed manifest projection receipt. Complete transitive closure topology also has a durable reuse record: each source content version is revalidated in resumable steps, and current closure limits must still permit reuse. A changed or missing version falls back to descriptor traversal. Snapshot construction still rechecks object generations and current availability; retained snapshots stay immutable and TTL is not extended to hide slow builds. Projection capture only retains a revision that remains unchanged from the start to the end of image construction. A completion cannot acknowledge a different source revision based on its timestamp.

Source scanning, image metadata/capture and snapshot assembly batch at most four transitions per checkpoint, further bounded by the configured batch size, cancellation and remaining execution time. A new graph capture identity is checkpointed before its immutable pages are written. Image metadata is checkpointed before immutable capture begins; subsequent payload writes can replay from that frozen metadata. Assembly checkpoints use the version read at the start of the unit and retain conditional-write and unknown-commit recovery checks. Routine refreshes also retain change-journal and dependency fanout progress. Unchanged image fragments reuse captured evidence while generation, binding identity, GC epoch and the open non-exclusive gate remain valid; journal changes invalidate affected image and content fragments. A full inspection checks source versions and registry existence after the configured fragment reuse interval, while content hashing is repeated after the configured content audit interval or when no current verification evidence exists. Original evidence times remain visible. Status includes incremental progress and the next full source reconciliation. Empty change journals or projection queues do not establish complete source coverage. Representative storage latency and discovery acceptance remain separate checks; see [inventory.md](inventory.md) for reconciliation and reuse limits.

Confirmed client manifest pushes save a durable notification for the affected repository. Routine refresh and a single pass before draft reference capture reconcile its paginated tags and content links before retiring the captured notification revision. The draft pass exposes `publicationReconciliation` progress and leaves unrelated source revisions pending. Publications arriving after that pass wait for a subsequent refresh. An adopted older reference cursor defers the pass rather than acknowledging bindings it might already have skipped. `TestInventoryClientPublicationReconcilesOnlyChangedRepository` exercises idle and active-draft pushes with an initially empty catalog, reloads every checkpoint and rejects a global rescan. Marker failure and older global notifications retain the checkpoint barrier before full reconciliation acknowledgement. Image construction refills free slots among four concurrent images while preserving each metadata barrier; large progress buffers defer additional images without truncation. Parallel capture regressions cover partial immutable writes, checkpoint replay and deferred progress recovery.

If a repository disappears before its notification is processed, a confirmed tags-list 404 completes that observation. Other repositories continue to progress. Tag pagination retains its cursor across restart, and acknowledgement preserves a newer publication notification received during the scan.

The larger historical regression covers 20 roots, 104 manifests, 303 edges and 50 blob records, including checkpoint timeouts and unknown commits. Its initial/refresh targets are five/three minutes, with continued binding writes, checkpoint reloads and leader takeover during refresh. These local checks do not establish a production S3 latency guarantee. Validate first publication, refresh duration and expiry windows against the deployment's inventory size, ongoing writes and storage latency before closing the large-scale K1 performance issue.

## Conditional manual retries

Read the job first, then submit its exact opaque revisions:

```http
POST /api/v1/background/download_cleanup/WORK_ID/retry
Content-Type: application/json

{
  "expectedRevision": "REVISION_FROM_GET",
  "sourceRevision": "SOURCE_REVISION_FROM_GET",
  "idempotencyKey": "operator-request-unique-id"
}
```

Only failed or retry-wait jobs are eligible. A durable acceptance returns 202 and a status link; execution is asynchronous. Reusing the same principal/idempotency key and identical input returns the existing acceptance. Before accepting new work, the handler checks authoritative generations and the required intent, root, retained manifest or cleanup ticket. Changed revisions, conflicting input, superseded sources or ineligible state return 409 `REVISION_CONFLICT`; absent required sources return 409 `SOURCE_MISSING`. An unavailable validation source returns 503. The acceptance protocol recovers a crash between the work CAS and acceptance confirmation. Generation, fencing, cache verification and anchor rules still run during execution. Bodyless retries remain available to legacy `task-admin` callers with a deprecation warning.

Retry records are retained for at least 30 days. The latest marker referenced by retained work is kept longer for crash recovery. Completion receipts normally last 30 days; execution receipts remain part of the durable lifecycle evidence.

## Event and transfer retention

Each task, intent and job carries a durable outbox of up to 32 state transitions, collected asynchronously into seven-day event history. The event log reserves at most 16 MiB per UTC day, with an 8 KiB event limit. Ring overflow and daily capacity rejection set explicit history-gap observations. Process/storage events use a bounded best-effort channel; `processDropped` reports lost events for the current instance. Collection cannot recover events predating deployment or states overwritten before collection. Events identify their attempt layer and report unknown timeout scope or responsibility when causality cannot be established. Tasks and intents retain their last safe error and recovery time independently of outbox rotation. A running/ready transition is not recovery; successful verification or completion establishes recovery. Background attempts retain parent attempt IDs, timing, runnable/enqueued/source ages and lease takeover events.

S3 transport observations retain failed requests and responses received after retries, with HTTP status, attempt number, response-header timing, request ID and available parent task/work correlation. Successful first requests and ordinary first-attempt not-found/conditional conflicts are aggregated in metrics. Diagnostic reads and event persistence suppress recursive attempt collection. These observations do not include credentials, object paths, signed URLs or response bodies. Receiving response headers does not prove that a streamed body was consumed or that the enclosing logical operation succeeded. SDK HTTP attempts remain separate from business retries; the existing retry/backoff policy is unchanged.

Upstream transport observations identify each registry/token HTTP attempt, including redirects, with a shared operation identity and a separate enclosing task/work attempt. Response headers and response-body completion are separate events. Body observations report bytes consumed, declared length when available, early close and read failure; EOF with an inconsistent declared size is not complete. Client upload proxy attempts similarly report response status or transport failure and request-body bytes consumed by the transport. These transport observations use the bounded best-effort event channel, so process interruption or collection limits can leave gaps. They never include bearer tokens, upload state tokens, private endpoints or response bodies. An observed source enqueue failure reports deferred handoff; a previously failed source entity emits a reconciliation recovery observation when it succeeds. No observation changes retry eligibility or backoff.

Transfers preserve original session times when available. Unknown byte counts remain null. Distribution writes expose a separately timed attempt with transport body bytes read, HTTP status, error and digest-qualified commit confirmation. Body bytes read do not prove remote acceptance; progress timestamps advance when bytes change or the attempt ends. Client uploads record accepted offsets from valid registry Range responses, request counts and response status; unknown or ambiguous final sizes remain null. Completed download cleanup means its guarded ticket completed or was superseded; the quota map independently establishes reservation release. Terminal transfer observations and upload receipts have a 30-day retention window after their last retained source observation. Physical temporary-byte associations come from a separate storage snapshot; no association is not proof of zero residual bytes.

## Physical storage reports

| Route | Contract |
| --- | --- |
| `GET /api/v1/storage/summary` | State and registry domains, mutually exclusive category totals, collection start/end, errors, reservation counts and coverage. |
| `GET /api/v1/storage/usage` | Descending physical object bytes by category, repository, image or platform. `dimension`, `imageID` and `platform` filters select groups. |
| `GET /api/v1/storage/objects` and `/{objectID}` | Object bytes, stable physical ID, observation/version identity, associations and protection/cleanup evidence. Private storage paths are excluded. |
| `GET /api/v1/storage/reclaimable` | The same object evidence with conservative candidate and eligible byte totals for the page. This endpoint authorizes no deletion. |

Usage queries group by `dimension=category|repository|image|platform`. Combine `category`, `repository`, `imageID`, `platform` and `domain` filters to narrow the group. Without an explicit dimension, a single filter selects its own dimension; multiple filters select category; an unfiltered query returns the independent single-dimension groups. Repository/image/platform intersections require one matching captured association. An object counts once per matching group, even when several associations match. Usage results are materialized during collection; GET performs no aggregation writes. Legacy retained reports support their original single-dimension queries and return 400 for unsupported intersections.

The collector persists leased checkpoints and publishes immutable object pages containing at most 32 objects or 1 MiB. Usage deltas are reduced and sorted through immutable runs with persisted merge cursors and at most 32 output rows per page. Each usage merge transition batches up to four deterministic merge operations, yielding before its deadline reserve; freshly inspected physical objects retain their separate checkpoint before immutable output. The checkpoint does not retain all groups, and new reports have no global 4,096 usage-group limit. Published usage pages contain at most 32 rows; GET reads one bounded page, so filters may yield empty continuation pages. The first scan starts promptly; completed scans restart after 15 minutes and reports expire after one hour.

The collector excludes its active snapshot directory. Other `diagnostics/v1/storage/` objects contribute metadata bytes and counts to the `storage_inventory_output` category and its category usage group. The state domain declares that category in `summaryOnlyCategories`: these internal objects have no materialized object rows, associations, repository/image/platform groups or reclaimable details. Their metadata deltas accumulate in the checkpoint and create one usage aggregate per collection, independently of the number of historical files or listing pages. Domain totals therefore include more objects than the detail endpoint enumerates. Inventory snapshots and other diagnostic metadata retain their own detailed categories. Legacy reports and in-progress legacy collections keep their original coverage and pages; new collections use summary-only coverage. Legacy reports omit cost evidence that was not collected.

The report's `cost` exposes listed input object counts/bytes, summary-only object counts, and pre-publication snapshot output object counts/bytes. Input includes entries excluded as active output; output includes attribution and intermediate files but excludes the publication manifest, current pointer and coordination checkpoints. These are logical metadata totals, not HTTP attempts or underlying disk usage. A bounded metadata pass measures output with a durable cursor. Collector status also reports committed cost, `budgetYields` and `lastYieldReason`; budget yields retain progress and do not publish an incomplete report.

Diagnostic retention conditionally removes obsolete usage runs from published reports, retaining the final usage run referenced by the manifest and all valid object pagination. Expired snapshot cleanup first persists manifest identity, expiry and a listing cursor under `diagnostics/v1/storage/retention/`, deletes children in batches of at most 32, then deletes the matching manifest and cleanup record. Restart and unknown deletion outcomes retain expiry evidence. Active collection checkpoints protect their output; unfinished output without a known manifest retains the separate 24-hour orphan grace. GET pagination binds the retained storage snapshot even if a newer report is published. Attribution uses a retained inventory snapshot, with up to 128 image associations per physical object; transfer association totals retain their 4,096-group bound. `coverage`, `reasons` and `truncated` accompany usage pages, including truncation in legacy reports. Expired attribution sources and association limits remain explicit; physical collection continues with unknown/partial attribution.

Physical bytes count each observed object once within its domain. Duplicate payloads in cache and Distribution occupy both domains. Logical bytes count the union of declared referenced objects in the captured inventory, including missing physical content. `residentPayloadBytes` counts observed payload bytes. Image/repository/platform groups overlap and cannot be added to obtain a domain total. Shared/exclusive bytes are relative to canonical image content IDs in the captured inventory; multiple tags for identical content do not create additional owners. Missing or truncated associations remain unknown. Category totals are mutually exclusive and can be summed to the domain total.

Logical referenced bytes do not have a physical storage category: groups with a category selector return `logicalBytes=null` and `logicalUnknown=true`. This avoids treating a physical-category subset as the logical image size. Combined groups without a category retain the captured logical object union when its metadata is complete.

Reservations are separate from object bytes. Underlying filesystem allocation, replicas/erasure coding, S3 noncurrent versions, delete markers and incomplete S3 multipart uploads remain unknown. Filesystem staging and lock files are outside visible object totals. Unassociated objects are counted even when their owner or cleaner cannot be determined. Candidate cache reclamation requires current lease, reader, protection, intent, projection and expiry checks; the actual cleaner must validate them again. Unknown candidates never increase confirmed reclaimable bytes.

Reclamation evidence distinguishes queued/running/retrying/failed download cleanup, idle-expired client upload metadata, event/receipt retention and GC protection barriers. Each known cleaner/work has a management link, progress and next retry when available. Execution receipts remain durable lifecycle evidence. A completed cleanup receipt does not prove that every object seen by a different scan has disappeared. Distribution upload purging is separate from blob GC; its policy and remote session liveness can remain unknown. Protected cache, unanchored Distribution and download reservations have separate capacity observations and limits, and never contribute to physical object byte totals.

## Registry inspection configuration

State storage is always inspected. Registry physical inspection defaults to `disabled`, producing an unavailable domain with null bytes and `inspection_not_configured`. Standard Registry APIs cannot enumerate all uploads and unreferenced storage objects.

For filesystem Distribution storage, mount the registry data root read-only into the backend:

```yaml
backend:
  registryInspection:
    config:
      backend: filesystem
      filesystemRoot: /inspection/registry
  extraVolumes:
    - name: registry-inspection
      persistentVolumeClaim:
        claimName: registry-data
        readOnly: true
  extraVolumeMounts:
    - name: registry-inspection
      mountPath: /inspection/registry
      readOnly: true
```

Select the actual Distribution PVC and data subdirectory. Its access mode and scheduling must allow the backend to read the same volume; configure placement/shared access as appropriate. State and registry roots must not overlap. The filesystem inspection adapter rejects writes.

For S3, use a separate read-only credential and the Distribution bucket/prefix:

```yaml
backend:
  registryInspection:
    config:
      backend: s3
      s3:
        endpoint: https://objects.example.org
        region: us-east-1
        bucket: registry
        prefix: distribution
        usePathStyle: true
    credentialsSecret: registry-inspection-credentials
```

The Secret supplies `REGISTRY_HUB_REGISTRY_INSPECTION_ACCESS_KEY_ID` and `REGISTRY_HUB_REGISTRY_INSPECTION_SECRET_ACCESS_KEY`. Connection defaults inherit the state S3 configuration, while bucket and prefix must be configured separately. Existing workload/default credential providers remain available when no static credential is supplied. Grant List/Get metadata access, with no write/delete privileges. The inspection wrapper rejects writes; collection never reads registry payload bodies. Backend configuration is also available directly through `REGISTRY_HUB_REGISTRY_INSPECTION_CONFIG` JSON. Inspection failures affect the registry domain report independently from state accounting and request handling.

## Verification

The independent `feature-diagnostics` and `feature-diagnostics-s3` cluster scenarios require completed accounting for both domains. They reconcile object/category pages, verify the uploaded fixture, independently count registry file bytes, and check retained report identity after a backend restart. The S3 scenario stores backend state in a dedicated fixture; registry inspection uses a read-only filesystem mount. See the [test guide](../tests/features/minimal/README.md).

The backend regression `TestInventoryRefreshWithAllStorageReadsDelayed` measures initial publication and refresh with one millisecond added to each GET, HEAD, PUT and LIST. It changes a reference during construction and reloads the committed checkpoint after every step. The default four-root case targets 60 seconds for initial publication and 30 seconds for refresh. Set `REGISTRY_HUB_TEST_INVENTORY_SCALE=1` for 20 completed roots, with targets of 180 and 120 seconds. It does not simulate every S3 latency distribution or the larger historical graph fixture.

`TestInventoryRefreshAgainstS3` runs the same workload against a dedicated test bucket when `REGISTRY_HUB_TEST_INVENTORY_S3_CONFIG` contains a JSON `config.S3` value. Supply credentials through the standard AWS environment/provider chain. The test creates and deletes only a unique prefix below the configured prefix; it never clears the bucket. Without the variable, it skips. These measurements do not establish a capacity guarantee for arbitrary inventories.

`TestStorageUsageMergeAgainstS3` uses the same dedicated-prefix lifecycle with `REGISTRY_HUB_TEST_STORAGE_USAGE_S3_CONFIG`. It verifies 4,227 usage groups across 1,056 image owners, checkpoint reload and deterministic replay, descending ranking, bounded pages, combined filters and read-only API enumeration. Its declared bound is 15 minutes for construction and enumeration. This measures the usage reducer against S3; the cluster scenarios separately exercise physical collection and report preservation.
