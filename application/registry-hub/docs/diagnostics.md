# Management diagnostics and storage inventory

All routes below use the private admin listener. Grant `observe` for reads and metrics, and `work-retry` for conditional background retries. Existing `task-admin` credentials include both permissions. Authentication and GC/write authorization remain independent. GET requests read bounded metadata; they do not fetch missing payloads, create protection sessions, repair indexes, or clean storage.

## Investigation routes

| Route | Evidence |
| --- | --- |
| `GET /api/v1/status` | Version, process instance, readiness, independently observed inventory, queues, statistics, probes and workers. |
| `GET /api/v1/images/status` | Durable build checkpoint, source scan, image/graph phase and background observations. Available before the first inventory publication. |
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

These shared budgets apply to admitted queues, source scans, statistics and maintenance. Independent probes, event collection, startup index repair and inventory construction report their own budgets without claiming shared concurrency. Inventory construction uses its configured batch timeout and commits within that batch. Protection recovery reports its observed execution budget; periodic and startup runs have different bounds.

Background leader acquisition also requires admission and has a five-second execution bound. Renewal and release use a reserved coordination budget of at most five seconds, independently of bulk capacity; renewal also stays within one quarter of the requested lease TTL. They retain the requested TTL and report `lease_renewal` or `lease_release` timeout scope when applicable. Failed renewal cancels leadership. Nested leases acquired within admitted work use the same finite coordination bounds. An execution timeout can still release its lease within the original lifecycle budget; shutdown starts no detached release requests and leaves held leases to expire. Initial scan-checkpoint reads have a separate five-second bound, and inventory builder initialization has an admitted 15-second bound.

Worker heartbeat, successful scan completion and entity progress have different meanings. Successful empty scans refresh `lastSuccessAt` and `lastScanCompletedAt` without inventing entity progress. A heartbeat alone does not prove completion. Scan freshness uses the last successful full round; storage collection uses a ten-minute scan validity, and ordinary scan/probe summaries use 90 seconds. Missing observations and read errors are explicit. A failed build checkpoint read makes build state unknown; an expired snapshot does not imply a build is running.

Projection reports distinguish `projection_pending`, `snapshot_pending`, `captured_as_observed` and `unknown`. Their coverage is a set of non-atomic source sweeps. Zero jobs cannot establish that every source commit has been observed, and matching revisions do not prove global catch-up. Source revisions are opaque identities, with no lexical ordering. The current lifecycle and a retained snapshot can legitimately disagree during projection delay.

The image projection response reads up to four independently indexed execution sources and four legacy captured root task IDs at the captured manifest generation. Follow `execution.factsURL` to enumerate the indexed scope. Root execution links are saved before execution. Shared blob links derive from durable demands and generation-qualified root/platform graph nodes; their start time is the later of the original blob execution and demand creation. They preserve the original blob task ID and separately identify the root task. Legacy or unreconciled associations remain unknown. `execution.knownFirstStartedAt` preserves the earliest time in the observed facts and captured lifecycle, while `capturedFirstStartedAt` belongs to the immutable snapshot. Missing indexed history does not prove execution never started.

Source reconciliation and history sweeps persist their cursor, buffered page, per-entity backoff and successful-round status under a renewed lease. Queue enumeration persists its partial summary and cursor together. Statistics persist their cursor, accumulators, invalid-round marker and scheduling backoff; after restart they restore the last complete metrics before resuming the partial round. Partial aggregates never become complete totals. Quota and filesystem orphan maintenance also retain their continuation and backoff. Checkpoints use CAS and fencing, and permit replay since the last checkpoint. Failed checkpoint writes relinquish leadership and reload durable state; an old lease owner cannot replace a newer checkpoint. Worker output identifies the lease owner, fence and `durable_checkpoint` scope. Index repair remains a startup operation, and periodic index maintenance explicitly reports `not_implemented`.

Source, statistics and maintenance checkpoints retain `lastError`, `recoveredAt` and bounded diagnostic outboxes. Recovery preserves the earlier failure and emits a separate event; unrelated entities and attempts are not coalesced. Protection recovery retains the same failure/recovery history in a separate record, using a finite checkpoint budget from the original lifecycle when available. Lease coordination and process observations use the best-effort event path. Store failure, interruption before checkpoint and outbox retention can still leave declared history gaps.

Dependency responses link the current intent, activation worker and observed notification work, including its checkpoint and next retry. Config and layer objects both receive live intent links. Task links require an existing task. Complete cache with a `waiting_cache` intent reports `activation_pending`. Notification coverage identifies the current revision without an excluded root; follow `notificationsURL` for indexed variants, including excluded-root work. Enqueue and history reconciliation maintain that index; missing legacy entries do not establish global completeness. Target pages traverse owner and cache-protection references and show actual wake work or `not_enqueued`/`excluded`/`unknown`. The same root can appear in both sources; `confirmedTargets` counts reference confirmations, not distinct roots.

Image snapshots retain parent digests from manifest metadata independently of task graph retention. `parentEdgesScope` distinguishes captured, partial and legacy unknown evidence. A shared object retains up to 32 parent digests and sets `parentEdgesTruncated` when more exist. Captured parent links do not silently change when live task graphs are cleaned; live intent and notification observations remain separately identified.

Build status reports source-scan start, current build start and elapsed seconds, the latest publication and complete publication times, previous build/refresh durations, and seconds until the current snapshot expires. A negative expiry interval preserves the actual expiry window. `localDirtyHint` only describes this process; it is not a complete source backlog. Source and graph checkpoint token fields contain opaque hashes for progress comparison.

Protection recovery reports its current phase and `full_rescan_on_restart` behavior. Necessary startup protection/index recovery and the GC barrier are preserved. The diagnostic endpoints do not make recovery resumable or bypass an unresolved barrier.

## Refresh performance acceptance

`TestInventoryRefreshLatencyWithStableContentAndChangingReference` defines a local regression workload of four completed roots and four distinct manifests/configs, their tag and digest references, and a reference updated every 25 checkpoint steps. It uses durable filesystem storage and loopback Distribution, with one millisecond of additional latency per graph edge read. The targets are a complete first publication within 60 seconds and a complete subsequent refresh within 30 seconds, with no edge rereads for unchanged terminal graphs. Every step reloads the persisted checkpoint, including during continued reference writes.

Terminal projection reuse requires the exact task storage revision, matching GC epoch and an open gate without recovery or cleanup. Changes invalidate reuse. Cached manifest topology reuse requires the same content generation and a completed manifest projection receipt. Complete transitive closure topology also has a durable reuse record: each source content version is revalidated in resumable steps, and current closure limits must still permit reuse. A changed or missing version falls back to descriptor traversal. Snapshot construction still rechecks object generations and current availability; retained snapshots stay immutable and TTL is not extended to hide slow builds. Projection capture only retains a revision that remains unchanged from the start to the end of image construction. A completion cannot acknowledge a different source revision based on its timestamp.

Source scanning, image metadata/capture and snapshot assembly batch at most four transitions per checkpoint, further bounded by the configured batch size, cancellation and remaining execution time. A new graph capture identity is checkpointed before its immutable pages are written. Image metadata is checkpointed before immutable capture begins; subsequent payload writes can replay from that frozen metadata. Assembly checkpoints use the version read at the start of the unit and retain conditional-write and unknown-commit recovery checks. Routine refreshes also retain change-journal and dependency fanout progress. Unchanged image fragments are reused only after source versions, listing membership, physical payload fingerprints, identity, closure limits and GC epoch validation; original evidence times remain visible. Status includes incremental progress, the next full source reconciliation, and fragment validation offsets. Empty change journals or projection queues do not establish complete source coverage. Representative storage latency and discovery acceptance remain separate checks; see [inventory.md](inventory.md) for reconciliation and reuse limits.

Confirmed client manifest pushes also save a durable catalog notification. The next refresh checkpoints a full reconciliation requirement before retiring that notification; restart cannot defer discovery until the five-minute periodic scan. `TestInventoryClientPublicationForcesDurableCatalogReconciliation` exercises the real manifest push path with an initially empty catalog and reloads every checkpoint. Marker-failure regression verifies the same checkpoint barrier before acknowledgement.

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

The collector persists leased checkpoints, processes at most four physical objects per step, and publishes immutable object pages. Usage deltas are reduced and sorted through immutable runs with persisted merge cursors and at most 32 output rows per page. Each checkpoint batches up to four deterministic merge transitions, yielding before its deadline reserve; freshly inspected physical objects retain their separate checkpoint before immutable output. The checkpoint does not retain all groups, and new reports have no global 4,096 usage-group limit. Published usage pages contain at most 32 rows; GET reads one bounded page, so filters may yield empty continuation pages. Completed scans restart after five minutes; reports expire after one hour. The collector excludes its active output directory to avoid counting a growing report recursively; prior reports are counted on the next sweep. GET pagination binds the retained storage snapshot even if a newer report is published. Attribution uses a retained inventory snapshot, with up to 128 image associations per physical object; transfer association totals retain their 4,096-group bound. `coverage`, `reasons` and `truncated` accompany usage pages, including truncation in legacy reports. Expired attribution sources and association limits remain explicit; physical collection continues with unknown/partial attribution.

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
