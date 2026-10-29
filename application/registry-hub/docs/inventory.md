# Image inventory

The management API exposes a persistent inventory of observed references, manifest content, and cache objects. All inventory routes require the existing `task-admin` permission. They use the management listener and authentication settings described in [security.md](security.md).

| GET endpoint | Result |
| --- | --- |
| `/api/v1/images` | Reference inventory; use `scope=platform` for independent manifest content |
| `/api/v1/images/summary` | Counts by scope and category, plus coverage and freshness |
| `/api/v1/images/{imageID}` | Binding, lifecycle, classification, and evidence |
| `/api/v1/images/{imageID}/platforms` | Descendant manifests, including artifacts with no known platform |
| `/api/v1/images/{imageID}/objects` | Deduplicated manifest/config/layer closure |
| `/api/v1/cache/objects` | Cache objects, including blobs without known manifest associations |

Lists accept `limit` (default 100, maximum 1000), `token`, and an optional `snapshotID`. Image lists support exact `category`, `authority`, `repository`, `reference`, `digest`, `platform`, `cacheState`, and `executionState` filters. Cache lists support `authority`, `repository`, `digest`, `cacheState`, `associated`, and `cleanupPending` filters. Each enum filter accepts one complete value; joined values such as `complete|partial` return 400. Booleans use `true` or `false`.

```sh
curl -u "$ADMIN_USER:$ADMIN_PASSWORD" "$ADMIN_URL/api/v1/images?category=cached_not_started"
curl -u "$ADMIN_USER:$ADMIN_PASSWORD" "$ADMIN_URL/api/v1/images?scope=platform&platform=linux/arm64/v8"
curl -u "$ADMIN_USER:$ADMIN_PASSWORD" "$ADMIN_URL/api/v1/cache/objects?associated=false"
```

Use returned `nextToken` values unchanged. Tokens bind the filters and immutable snapshot, expire after at most 15 minutes, and allow changing the page size. Details and child lists accept `snapshotID` to inspect the same observation. Invalid queries return 400, missing image IDs return 404, expired snapshots/tokens return 410, and an unavailable core index returns 503. Responses use the existing management error envelope.

## State interpretation

`cache.state` measures verified cache availability independently of Distribution. Its values are `complete`, `partial`, `none`, and `unknown`. `availability.state` measures the union of cache and verified local Distribution objects. Counts and bytes describe a deduplicated logical closure within one scope; totals across images are not physical storage usage.

Categories follow this priority: `solidified`, `cached_not_started`, `solidifying`, `failed`, `cancelled`, `cached_start_unknown`, `waiting_cache`, `not_scheduled`, and `unknown`. Solidified content requires a complete verified closure and durable anchor evidence for the current content generation and GC epoch. A tag also requires a confirmed local binding. A successful task or a manifest HEAD response alone does not establish solidification. One platform can be solidified while its parent index remains incomplete.

`solidification.started` is tri-state. A reliably registered new lifecycle starts at `false`; a worker durably records `true` before local validation, transfer, or publication. Queueing and upstream discovery do not count as starting. Persistence failure defers that worker attempt. Historical scopes without reliable start evidence report `null` and `legacy_start_unknown`. Retries and GC do not reset the first start time. A tag binding revision and digest content generation have separate lifecycle identities.

Unknown platforms remain null. Referenced attestations and other artifacts remain part of an index closure. A manifest's subject association is metadata and does not implicitly expand the dependency closure. Locally pushed repositories without an upstream authority use an empty `authority` and their original repository name.

## Storage and refresh

Inventory records, restart checkpoints, and immutable snapshot pages use `inventory/v1/` in the existing backend store. No additional database, bucket, or service is needed. A fenced builder lease coordinates replicas. Task cleanup hands off durable content and anchor facts before deleting the graph; lifecycle and inventory facts survive task retention.

Successful source writes request a refresh. Periodic scans recover missed notifications and crashes. Discovery reads cache metadata, task graphs, and the authenticated internal Distribution catalog/tags API. Bounded local manifest reads can discover dependencies; the inventory never fetches missing upstream content. Catalog/tags cannot enumerate historical untagged manifests that have lost all other associations. Responses explicitly include `untagged_history_not_enumerable`; coverage applies only to discoverable sources. Orphan blobs expose their object identity without inventing an image name.

During initial backfill, responses expose `coverage=partial` and `building=true`. Queries only read published snapshot records. They do not call Distribution or upstreams, create reader pins or tasks, change protection owners, or clean caches. A stale snapshot retains the classifications observed at its reported time; it does not claim current success. After a generation or GC epoch change, old proof cannot establish solidification until current verification and anchor evidence exists. Read failures and corrupt or missing cache payloads carry stable reason codes.

Snapshot construction captures immutable records before assembling list pages. Each image becomes a ready member only after its object and platform-link pages are durable. A persistent dependency queue includes resolved reference roots and descendant manifests, even if they arrive after the initial content scan. Missing source content produces an unknown platform member with closure reasons and any known descriptor metadata. Every such member appears once in platform lists and counts.

After dependency completion, the builder seals capture and validates links while assembling pages in checkpointed batches. Assembly retries reuse captured records and committed pagination progress. Both complete and partial snapshots require all platform links to resolve within the same snapshot before publication; a failed build leaves the previous published snapshot available. New source observations are picked up by a later build. Published snapshots remain immutable throughout retention.

Cache inspection checks complete markers, size, and payload version without rereading layers. Filesystem stores use a fingerprint captured at verified cache write time; historical data without that fingerprint remains unknown until normal cache verification establishes it. Bounded historical manifest reads may recover closure metadata without creating reader pins. When previous local verification is unavailable, the background builder can stream and hash existing Distribution content once, persisting that audit separately from task observations. A complete locally verified closure and a confirmed published tag establish durable publication proof. This audit never marks a solidification lifecycle as started.

| Helm value under `backend.inventory` | Environment variable suffix after `REGISTRY_HUB_INVENTORY_` | Default | Bounds |
| --- | --- | --- | --- |
| `refreshSeconds` | `REFRESH_SECONDS` | 30 | 5–3600 |
| `staleSeconds` | `STALE_SECONDS` | 120 | At least refresh interval, at most 86400 |
| `snapshotRetentionSeconds` | `SNAPSHOT_RETENTION_SECONDS` | 3600 | 900–604800; greater than stale threshold |
| `batchSize` | `BATCH_SIZE` | 100 | 1–1000 |
| `batchTimeoutSeconds` | `BATCH_TIMEOUT_SECONDS` | 60 | 1–600 |

The builder processes bounded batches, persists scan cursors, and retries failures with exponential delay up to five minutes. Snapshot age starts at build start rather than publication, so a slow scan cannot make old evidence appear newly observed. Retention starts at publication so a slow build still provides a usable pagination window. Expired snapshots are removed in resumable batches. Superseded terminal task projections are removed after 30 days once the original task is gone; the latest outcome and independent content, lifecycle, and anchor facts remain. Responses include at most 32 related task records, prioritize active work, and report `taskCount` and `task_history_truncated` when more history exists. Full current task diagnostics remain available through the task API.
