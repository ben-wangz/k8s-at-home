# feature-minimal Regression Suite

This directory provides the independent cluster regression entry point for Registry Hub. The test program is a Go module under tests/, separate from the product source in src/. Build outputs go to the user cache and are not included in product images, charts, or release workflows.

This is the feature-minimal scenario. It uses empty Helm values to verify default behavior. Deployable test scenarios are organized as features; each scenario keeps its config.yaml beside its Go code.

coverage.json contains 111 stable baseline identifiers, case and unit-test mappings, and feature-scenario mappings. All 111 entries have acceptance evidence and are classified as complete. Each case.json declares individual checks, and the runner rejects a passing report that lacks check results or evidence.

Product tests named `TestProtectionRecovery*` cover recovery across execution budgets and process takeover, existing reference indexes, checkpoint failure after every work unit, terminal demand deletion with unknown commit, stale fences, reservation cleanup, and remote blob hash checkpoints. They also verify that a failed completion checkpoint is retried after the gate commit and that quiescence honors cancellation. Run them with `go -C application/registry-hub/src test -race ./internal/app -run TestProtectionRecovery -count=1`; `cmd/registry-hub` and storage probe tests cover startup health and preservation of storage error causes. Cluster cases `restart-reuse`, `podman-upstream-pull`, and `feature-backend-s3` exercise restart, actual client, and storage integration paths.

Cache I/O regressions include strict verify-only and consumption byte bounds, cancellation during hashing, descriptor replacement, conditional S3 consumption, SDK retry body accounting, and capability forwarding through the complete observation/recovery/backend production chain. Internal readiness tests require zero payload opens and durable legacy verification. Discovery regressions cover immutable edge reuse, missing protection and edge repair, corrupt optional state, changed limits, shared dependency ancestors, independent platforms, missed hints, and GC epoch invalidation. Run the focused race checks with `go -C application/registry-hub/src test -race ./internal/artifact ./internal/storage/... ./internal/workqueue ./internal/app -run 'TestCache|TestVerifiedCache|TestObservedStore|TestProductionWrapperChain|TestS3Payload|TestS3ConditionalConsumption|TestPayloadClaim|TestReadinessUses|TestCompletedDiscovery|TestDiscoverySnapshot|TestDependencySignal|TestSharedDependency|TestGraphIncremental' -count=1 -timeout 30m`. Large generated fixtures are opt-in; see [Diagnostics](../../../docs/diagnostics.md#refresh-performance-acceptance) for the 64/256/1024 MiB paired measurement command and its limits.

Write-time integrity coverage also exercises stale staging cleanup, optional S3 checksum fallback, full-object multipart SHA-256 evidence, one-time legacy proof upgrade, and real Podman rejection of a blob with a wrong digest. Run the storage regression set with `go -C application/registry-hub/src test ./internal/artifact ./internal/storage/... -count=1 -timeout 20m`, the race subset with `go -C application/registry-hub/src test -race ./internal/artifact ./internal/storage/... -run 'Test(Cache|IntegrityReader|PutVerified|Replay|S3Checksum|ChecksumReadback)' -count=1 -timeout 30m`, and the isolated client check with `go -C application/registry-hub/tests test ./features/minimal/cases/podman-upstream-pull -run TestPodmanRejectsValidlyEncodedBlobWithWrongDigest -count=1 -timeout 3m`.

Download-cleanup regressions require one full cache verification per protected attempt across multiple batches, durable progress, fresh verification on retry/restart, retained quota until completion, and prompt rejection of lost pins, changed cache/session identities, recovery/deletion changes, and stale queue/object fences. Run `go -C application/registry-hub/src test -race ./internal/artifact ./internal/app ./internal/download ./internal/workqueue -run 'TestVerifiedPin|TestDownloadCleanup|TestCommittedCacheSurvivesDownloadCleanup|TestOldCleanupTicket|TestCommittedCleanup|TestRunningCheckpoint' -count=1 -timeout 10m`.

`TestTerminal*` additionally covers ordinary cleanup across short budgets and restart, expiry, missed enqueue repair, shared-demand admission, stale work fences, crash journals, and known-write checkpoint failures. Normal yields must preserve an open gate; unresolved writes must retain protection and recover. `TestRecoveryTrigger*` and `TestRecoveryClaim*` check trigger preservation through takeover and failed barrier writes. `TestProbeCleanup*` checks joined error causes and lifecycle cancellation. `TestProcessRevalidatesStaleDispatchUnderLease` verifies that stale worker dispatch cannot rerun terminal tasks, bypass dependency or capacity waits, or start a scheduled retry early. Run these product regressions with `go -C application/registry-hub/src test -race ./internal/tasks ./internal/app ./internal/storage ./internal/workqueue -run 'TestProcessRevalidates|TestTerminal|TestRecoveryTrigger|TestRecoveryClaim|TestProbe|TestRunningCheckpoint' -count=1`. Cancellation cluster checks already poll for terminal cleanup completion after an accepted cancellation response.

## Run

The scripts work from any current directory. Examples below assume the repository root:

    application/registry-hub/tests/features/minimal/run.sh list
    application/registry-hub/tests/features/minimal/run.sh plan
    application/registry-hub/tests/features/minimal/run.sh check
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-minimal

With no case selection, the runner executes all registered cases. Selected cases include their dependencies and run serially in a deterministic order. A case can also declare `runAfter` constraints; these affect ordering only when both cases are selected and do not add the earlier case to a selected plan:

    application/registry-hub/tests/features/minimal/run.sh run --case registry-roundtrip --case restart-reuse

Use --id <id> to choose a run ID. The runner generates and prints one when omitted. Do not reuse an existing ID for a new run. Failed runs retain the environment for inspection and explicit reuse:

    application/registry-hub/tests/features/minimal/run.sh environment inspect --id <id>
    application/registry-hub/tests/features/minimal/run.sh run --id <id> --reuse --case registry-roundtrip
    application/registry-hub/tests/features/minimal/run.sh environment destroy --id <id>

Reuse recalculates and runs the full dependency set for the selected cases. Create a new ID if the configuration or product source differs from the original run. To create the environment without running cases:

    application/registry-hub/tests/features/minimal/run.sh environment create --id <id>

Available scenarios are feature-minimal, feature-quota-convergence, feature-client-auth-catalog, feature-upstream-auth-transport, feature-backend-s3, feature-diagnostics, feature-diagnostics-s3, feature-registry-s3, feature-storage-redirect, feature-referrers-fallback, and feature-gc. They share one runner, dependency DAG, and test namespace. Each scenario has its own Helm values, release, and state directory:

    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-minimal
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-minimal
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-quota-convergence
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-client-auth-catalog
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-client-auth-catalog
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-client-auth-catalog
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-upstream-auth-transport
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-upstream-auth-transport
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-upstream-auth-transport
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-backend-s3
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-backend-s3
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-backend-s3
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-registry-s3
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-registry-s3
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-registry-s3
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-storage-redirect
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-storage-redirect
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-storage-redirect
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-referrers-fallback
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-referrers-fallback
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-referrers-fallback
    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-gc
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-gc
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-gc

feature-backend-s3 uses a VersityGW v1.8.0 fixture pinned by manifest-list digest. It creates a Pod, Service, run-specific random credentials Secret, and bucket. Preflight checks cover S3 SigV4, bucket creation, and conditional object operations. The case verifies backend S3 writes and task/artifact persistence after a backend restart. It interrupts an upstream Blob transfer, verifies persisted download state and chunks in S3, retries from the stored Range offset, and confirms that the completed cache object remains while temporary download objects are reclaimed. Transfer and asynchronous completion each have a five-minute correctness budget. Snapshot comparison permits progress in task, recovery, and inventory-retention checkpoints while still requiring those durable records to exist; immutable payloads remain byte-for-byte protected, and the selected task and manifest are checked before and after restart. The fixture uses HTTP, path-style addressing, and emptyDir. Adapter tests cover multipart operations, conditional-write conflicts and concurrency, and unknown commits. TLS/CA behavior and S3 server interruption are outside this scenario.

feature-registry-s3 switches Distribution artifact storage to S3 while the backend remains on FileSystem. The runner creates a TLS-enabled VersityGW Service, random credentials, a service certificate, and a private CA. The chart mounts the CA read-only and uses SSL_CERT_FILE so Distribution validates it. The case uploads and reads OCI blobs, config, and manifests; checks rejection of an invalid CA or credentials; checks object-prefix isolation and two-part multipart upload; and verifies content after a Registry restart. The fixture uses emptyDir and does not test persistence across a storage-server restart. Passing runs clean up automatically; failed runs retain the environment.

feature-storage-redirect uses the same run-specific TLS S3 fixture, enables Distribution redirects, and adds the endpoint to the allowlist. After uploading a local OCI fixture, the case reads a blob and verifies that a private address resolved from the S3 Service DNS name is rejected with 502. The response must not expose a signed URL, hostname, or Location header, and must not fall back to an upstream. Distribution client unit tests also verify successful allowlisted reads, per-hop validation, and credential stripping.

feature-referrers-fallback pins Distribution to the Registry 2.8.3 manifest-list digest from the mirror. A separate probe Pod checks that tags/list returns 200 for an existing repository while native /referrers returns 404. The case then verifies fallback OCI index writes and reads, tag and digest protection, and an empty index after deleting a referrer. Passing runs remove the probe and Helm environment; failures retain probe logs. The minimal referrers-migration case and application unit tests cover native authority, migration, repair replay, and protection against corrupt indexes.

feature-gc enables the Kubernetes GC Job and verifies draining, single-writer enforcement, PVC and ingress isolation, Job failure and explicit retry, failed_unknown recovery after a backend restart, object reclamation, and GC metrics.

feature-diagnostics enables read-only inspection of the Distribution PVC. It requires a completed report for both storage domains, enumerates every object and category usage page, reconciles their totals, checks the uploaded manifest/config/layer, and compares registry totals with an independent filesystem metadata scan. It restarts the backend and reads the same retained report again. Unavailable accounting fails this case. The backend and registry must be able to mount the same test PVC.

feature-diagnostics-s3 runs the same strict accounting and restart checks with backend state in the isolated S3 fixture. Distribution storage remains on the inspected PVC. This scenario does not verify S3 provider disk overhead, historical versions, delete markers, or storage-server durability.

    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-diagnostics
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-diagnostics-s3

referrers-migration uses the same empty Helm configuration as feature-minimal. It creates a run-specific Zot OCI Distribution 1.1 fixture pinned by digest, temporarily switches the Registry Service selector, and verifies migration from a legacy fallback index to native Referrers. It also checks read authority before and after migration, generation handling, and fallback tag protection. The case restores the original Service selector and removes the fixture. It is a minimal case, not part of feature-referrers-fallback.

upstream-mirror configures an HTTP proxy for external upstreams and adds localhost, .svc, and .cluster.local to NO_PROXY so in-cluster Service requests, including the S3 backend, remain direct. The case verifies Docker/OCI manifest discovery Accept values. If an upstream first returns an OCI index when the client requests a Docker manifest list, it makes one exact Accept retry and checks the response bytes, digest, original discovery root, nested three-level index, platform child manifest, config graph, and verified checkpoints. A client blob GET supplies the missing config and allows the root to complete; compatibility retries do not replace the discovered representation.

The backend S3 persistence scenario uses `upstream-persistence-fixture` to construct one completed image instead of depending on the full upstream matrix. Its healthy solidification has a 90-second bound. The full matrix remains in the minimal and Registry S3 scenarios, and `feature-upstream-matrix-s3` explicitly runs it with S3 backend storage and accumulated fixture state. `feature-upstream-latency` and `feature-upstream-latency-s3` isolate healthy completion and the default depth boundary on filesystem and S3 storage. They retain the five-minute depth assertion and save before/after metrics and worker observations. The full matrix also captures metrics and workers on failure within a separate ten-second diagnostic budget. Task polling uses a known task ID with bounded graph and owner pages, then retrieves full terminal details for assertions. Use the same immutable image and serialized runs when comparing storage backends or revisions.

Upstream fixtures run a precompiled binary in an image built and pushed by Podman. The image cache key includes sorted production fixture sources, the Containerfile, the pinned Go builder, and `REGISTRY_HUB_TEST_PLATFORM`. Pods use the pushed immutable digest; runtime credentials and CA bundles remain mounted from the fixture Secret and ConfigMap. The first build fills the Go compilation cache; subsequent scenarios reuse it instead of compiling inside each Pod. The `fixture-image` phase records this cost separately. These cases require Podman even when the backend is supplied with `--image`. The same binary provides a `seed-tasks` mode for the quota init container; it accepts projected ConfigMap records and publishes complete files without replacing existing task state.

The explicit `feature-large-layer` and `feature-large-layer-s3` scenarios measure streaming 64/256/1024 MiB layers in that order. Run them serially with the same immutable backend image and resource limits. They hold a protection-session slot during the initial download and verified cache read, cancel that blocker, then observe platform anchoring and a Distribution hit. `measurements.json` separates request durations, intentional hold time, and release-to-anchor time; cancellation, admission, and polling are included in the last interval. The manifest request and cold layer request are measured separately around setup rollouts. OS page-cache temperature is uncontrolled. Per-phase metrics, worker snapshots, task timelines, and backend performance logs are retained. Builds and settings rollouts are outside payload timings; no additional upstream layer GET is permitted on either subsequent hit.

Long upstream phases write `phases.jsonl` events and console progress before and after execution, including failures. The evidence distinguishes fixture setup, healthy completion, discovery, depth traversal, compatibility, supplemental checks, limit/fault checks, settings rollouts, readiness, and the resume suite's protected-write settling. Phase timings overlap when one phase contains another; do not add nested timings together.

Asynchronous task completion is generally polled for up to five minutes. The background retry check allows ten minutes because earlier error fixtures leave asynchronous cleanup ahead of the periodic quota sweep. Unexpected terminal states fail immediately; digest, graph, checkpoint, and upstream-request assertions still apply. These correctness timeouts are separate from inventory and physical-accounting performance acceptance targets.

The multipart probe gives its six-MiB client transfer five minutes, matching the default server request budget. Other requests keep their existing client limits; transfer size, digest, root publication, and upstream demand checks remain mandatory.

The upstream error matrix covers 401, 403, 408, 429, 500, 502, 504, and connection interruption. A 401 Basic challenge is not forwarded. Requests for 408, 429, 502, 504, and connection interruption are retried three times; 500 is requested once and is not treated as a miss. A separate assertion covers three retries for an existing 503 case. Retry-After coverage verifies two 429 responses with Retry-After: 5 followed by success, more than nine seconds of total waiting, and completion from a previously transferred config without another upstream blob fetch. A client-triggered config read verifies three 503 retries followed by success, then checks the solidified task, checkpoint, and local blob.

A controlled slow upstream verifies that concurrent waiters for the same authority/repository/digest do not duplicate a download. The waiter returns 503 with Retry-After: 5 after about 30 seconds; the leader then returns the complete blob. The deadline checks temporarily set the test instance request timeout to 15 seconds so S3 state admission can finish before the upstream wait. They require exactly one upstream request despite Retry-After: 60, a 504 response within the configured budget plus ten seconds, and no published task root. HEAD also requires an empty response body and no created task. The full upstream case allows 45 minutes for its error matrix, asynchronous completion, and repeated controlled backend restarts; this is a correctness budget, separate from performance acceptance targets.

The case verifies the default closure depth of 64: 64 validated child nodes retain checkpoints, while an over-depth root is not published and its first out-of-range child is not requested. A separate controlled low-threshold run sets the object limit to 3 and closure byte limit to 1. It verifies permanent closure_limit failure, no root publication, and no root checkpoint. Default Helm rendering leaves these environment variables unset. Go configuration unit tests verify defaults of 10,000 objects, 1 TiB of closure bytes, depth 64, and a 300-second request timeout. Invalid upstream descriptor.data Base64, size, digest, and media type each return 502 without publishing a solidified root. Client Range requests are served from a complete upstream GET that does not send a Range header.

upstream-resume uses deterministic OCI layers served by the same controlled proxy. It records actual Range offsets and sent bytes while testing first-response interruption, repeated short ranges, per-attempt and idle timeouts, slow full 200 responses, ignored Range fallback, ETag changes, 416, concurrent readers, client-triggered retry, and Backend restart. The retry case confirms discovery does not fetch the layer, then reads config and layer through the client API and verifies the same committed Range offset is retried after 503 responses. A separate Podman pull verifies the Service endpoint end to end. Temporary timeout changes are restored to defaults even when a check fails.

feature-quota-convergence runs the quota-convergence case in a separate release so its historical records and controlled restarts do not change the baseline inventory fixture. It seeds 100 historical completed task records through a temporary init container in its isolated Recreate deployment. The init container writes the fixture records before the backend starts and is removed before measurement. The case occupies the single permitted protection-session slot with an unrelated blob, then queues a small linux/amd64 manifest with an inline verified config before caching its layer, preserving FIFO admission order. Setup uses one task worker; it restores the default four task workers before verifying both cached blobs through client reads. Measurement retains the default two ordinary background slots and performs no backend restart. It cancels the blocker and allows five minutes for setup cleanup and completion, while requiring anchoring within 120 seconds of the committed covering-session association. Evidence records association, wake, execution and anchor times, zero upstream config GETs, and one upstream layer GET. This healthy-path bound is separate from storage fault recovery and inventory/accounting acceptance.

For failed feature scenarios, use environment inspect or destroy with the run ID. The run record recovers the scenario and release from that ID.

See the [environment guide](environment/README.md) for prerequisites and environment management.

## Registered Cases

| ID | Coverage | Execution constraints |
| --- | --- | --- |
| default-render | Deterministic empty-values rendering, paired external-auth validation, and admin/client Service readiness routing | None |
| installation | Initialization Job, default PVCs, Deployment, backend digest, and client Service | default-render |
| initialization-secrets | Internal Registry authentication, TLS, and initialization records | installation |
| initializer-faults | Production initialization command concurrency, errors, certificate checks, and recovery in an isolated Kubernetes API simulator | installation |
| admin-auth | Admin authentication Secret, permissions, and authenticated documentation endpoint | installation |
| rbac-boundary | Initialization Role scope, separation of workload identities, and Registry token settings | installation |
| startup-wait | Init container blocks startup while initialization is pending; Deployment becomes Ready after recovery | installation |
| registry-roundtrip | OCI content upload/read and digest consistency | admin-auth, initialization-secrets |
| image-inventory | Inventory discovery of pushed images, local closure proof, authentication, immutable pagination, and query validation | registry-roundtrip |
| feature-diagnostics | Complete physical accounting, independent registry totals, pagination, and retained reports after restart | registry-roundtrip, image-inventory |
| client-interop | Real Podman image and Helm OCI chart round trips | installation |
| restart-reuse | Job recreation, repeated Helm upgrades, Pod restart, release reinstall, and credential stability | registry-roundtrip |
| external-combinations | Automatic/external Secret combinations, cleanup simulation, reuse-values upgrade from old references, and restore to empty values | restart-reuse |
| registry-protocol | Registry protocol basics, error envelopes, HEAD, and listener boundaries | external-combinations |
| routing-paths | Authority-like repository path mapping, isolation, and invalid paths | registry-protocol |
| blob-upload | Upload sessions, chunks, invalid offsets, cancellation, and single-request uploads | registry-protocol |
| blob-mount | Cross-repository blob mount and fallback | blob-upload |
| blob-read | Blob GET, HEAD, Range, and unsatisfiable ranges | blob-upload |
| manifest-validation | OCI/Docker manifest validation, digests, and invalid input | blob-upload |
| tag-publication | Idempotent tags, conflicts, multiple tags, and concurrent publication | manifest-validation |
| tags-pagination | tags/list ordering, pagination, Link headers, and parameter boundaries | tag-publication |
| artifact-roundtrip | Multiple OCI artifacts, multi-architecture indexes, and subject fixtures | manifest-validation |
| referrers-read | Native Referrers, filtering, pagination, and cursors | artifact-roundtrip |
| referrers-lifecycle | Referrer deletion, subject retention, and native collection changes | referrers-read |
| referrers-migration | Successful migration from fallback index to native Referrers using a run-specific Zot fixture | installation |
| delete-semantics | Tag, manifest, and blob deletion; non-cascading semantics | artifact-roundtrip |
| security-defaults | Internal TLS/authentication, NetworkPolicy configuration, and SSRF failure tasks | routing-paths |
| admin-api | Admin authentication, tasks, reconcile, disabled GC, diagnostic API contracts and available storage snapshot pagination | security-defaults, referrers-read |
| artifact-persistence | Persistence of content, referrers, failed tasks, Secrets, records, and PVCs across restart/upgrade | artifact-roundtrip, referrers-read, tag-publication, admin-api |
| dependency-failure | Distribution scale-down, diagnostic Service, backend errors, and recovery | artifact-persistence |
| default-limits | Default request-rate limits for manifests, descriptors, uploads, tags/list, and unauthenticated requests | manifest-validation, tags-pagination |
| observability | JSON logs, Prometheus metrics, low-cardinality labels, and sensitive-data scans | dependency-failure, default-limits, referrers-lifecycle, delete-semantics |
| podman-upstream-pull | Podman pulls a pinned image from the NJU GHCR mirror by tag, index digest, and platform manifest digest, then rejects a controlled wrong-digest blob | installation; runs after restart-reuse and upstream-resume when they are in the plan |
| upstream-mirror | Local-first behavior, Docker/OCI media negotiation, upstream error matrix, descriptor.data/urls, nested multi-platform index graphs, original-field solidification, non-recursive subjects, HEAD metadata/cache boundaries, shared digest conflicts, default closure depth, controlled closure/object-byte limits, Range/If-Range, and digest validation | installation |
| upstream-resume | Digest verification, persisted offsets, repeated interruption, timeouts, restart and retry recovery, concurrency, Range fallback, and a real Podman pull | upstream-mirror |
| quota-convergence | Cached config/layer convergence with covering capacity occupied and at least 100 unrelated task records; durable scheduling timestamps | installation |
