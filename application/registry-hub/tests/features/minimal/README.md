# feature-minimal Regression Suite

This directory provides the independent cluster regression entry point for Registry Hub. The test program is a Go module under tests/, separate from the product source in src/. Build outputs go to the user cache and are not included in product images, charts, or release workflows.

This is the feature-minimal scenario. It uses empty Helm values to verify default behavior. Deployable test scenarios are organized as features; each scenario keeps its config.yaml beside its Go code.

coverage.json contains 111 stable baseline identifiers, case and unit-test mappings, and feature-scenario mappings. All 111 entries have acceptance evidence and are classified as complete. Each case.json declares individual checks, and the runner rejects a passing report that lacks check results or evidence.

## Run

The scripts work from any current directory. Examples below assume the repository root:

    application/registry-hub/tests/features/minimal/run.sh list
    application/registry-hub/tests/features/minimal/run.sh plan
    application/registry-hub/tests/features/minimal/run.sh check
    application/registry-hub/tests/features/minimal/run.sh run --scenario feature-minimal

With no case selection, the runner executes all registered cases. Selected cases include their dependencies and run serially in a deterministic order:

    application/registry-hub/tests/features/minimal/run.sh run --case registry-roundtrip --case restart-reuse

Use --id <id> to choose a run ID. The runner generates and prints one when omitted. Do not reuse an existing ID for a new run. Failed runs retain the environment for inspection and explicit reuse:

    application/registry-hub/tests/features/minimal/run.sh environment inspect --id <id>
    application/registry-hub/tests/features/minimal/run.sh run --id <id> --reuse --case registry-roundtrip
    application/registry-hub/tests/features/minimal/run.sh environment destroy --id <id>

Reuse recalculates and runs the full dependency set for the selected cases. Create a new ID if the configuration or product source differs from the original run. To create the environment without running cases:

    application/registry-hub/tests/features/minimal/run.sh environment create --id <id>

Available scenarios are feature-minimal, feature-client-auth-catalog, feature-upstream-auth-transport, feature-backend-s3, feature-registry-s3, feature-storage-redirect, feature-referrers-fallback, and feature-gc. They share one runner, dependency DAG, and develop namespace. Each scenario has its own Helm values, release, and state directory:

    application/registry-hub/tests/features/minimal/run.sh list --scenario feature-minimal
    application/registry-hub/tests/features/minimal/run.sh plan --scenario feature-minimal
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

feature-backend-s3 uses a VersityGW v1.8.0 fixture pinned by manifest-list digest. It creates a Pod, Service, run-specific random credentials Secret, and bucket. Preflight checks cover S3 SigV4, bucket creation, and conditional object operations. The case verifies backend S3 writes and task/artifact persistence after a backend restart. The fixture uses HTTP, path-style addressing, and emptyDir. Adapter tests cover multipart operations, conditional-write conflicts and concurrency, and unknown commits. TLS/CA behavior and server interruption are outside this scenario.

feature-registry-s3 switches Distribution artifact storage to S3 while the backend remains on FileSystem. The runner creates a TLS-enabled VersityGW Service, random credentials, a service certificate, and a private CA. The chart mounts the CA read-only and uses SSL_CERT_FILE so Distribution validates it. The case uploads and reads OCI blobs, config, and manifests; checks rejection of an invalid CA or credentials; checks object-prefix isolation and two-part multipart upload; and verifies content after a Registry restart. The fixture uses emptyDir and does not test persistence across a storage-server restart. Passing runs clean up automatically; failed runs retain the environment.

feature-storage-redirect uses the same run-specific TLS S3 fixture, enables Distribution redirects, and adds the endpoint to the allowlist. After uploading a local OCI fixture, the case reads a blob and verifies that a private address resolved from the S3 Service DNS name is rejected with 502. The response must not expose a signed URL, hostname, or Location header, and must not fall back to an upstream. Distribution client unit tests also verify successful allowlisted reads, per-hop validation, and credential stripping.

feature-referrers-fallback pins Distribution to the Registry 2.8.3 manifest-list digest from the mirror. A separate probe Pod checks that tags/list returns 200 for an existing repository while native /referrers returns 404. The case then verifies fallback OCI index writes and reads, tag and digest protection, and an empty index after deleting a referrer. Passing runs remove the probe and Helm environment; failures retain probe logs. The minimal referrers-migration case and application unit tests cover native authority, migration, repair replay, and protection against corrupt indexes.

feature-gc enables the Kubernetes GC Job and verifies draining, single-writer enforcement, PVC and ingress isolation, Job failure and explicit retry, failed_unknown recovery after a backend restart, object reclamation, and GC metrics.

referrers-migration uses the same empty Helm configuration as feature-minimal. It creates a run-specific Zot OCI Distribution 1.1 fixture pinned by digest, temporarily switches the Registry Service selector, and verifies migration from a legacy fallback index to native Referrers. It also checks read authority before and after migration, generation handling, and fallback tag protection. The case restores the original Service selector and removes the fixture. It is a minimal case, not part of feature-referrers-fallback.

upstream-mirror configures an HTTP proxy for external upstreams and adds localhost, .svc, and .cluster.local to NO_PROXY so in-cluster Service requests, including the S3 backend, remain direct. The case verifies Docker/OCI manifest discovery Accept values. If an upstream first returns an OCI index when the client requests a Docker manifest list, it makes one exact Accept retry and checks the response bytes, digest, original solidification root, nested three-level index, platform child manifest, config graph, and verified checkpoints. A failed compatibility request does not prevent valid discovery-root solidification.

The upstream error matrix covers 401, 403, 408, 429, 500, 502, 504, and connection interruption. A 401 Basic challenge is not forwarded. Requests for 408, 429, 502, 504, and connection interruption are retried three times; 500 is requested once and is not treated as a miss. A separate assertion covers three retries for an existing 503 case. Retry-After coverage verifies two 429 responses with Retry-After: 5 followed by success, more than nine seconds of total waiting, and a solidified root. A background-recovery case verifies that a config blob returning 503 three times in the foreground is recovered by retry_wait and ends with the expected task, checkpoint, and local blob.

A controlled slow upstream verifies that concurrent waiters for the same authority/repository/digest do not duplicate a download. The waiter returns 503 with Retry-After: 5 after about 30 seconds; the leader then returns the complete blob. The case temporarily sets the test instance request timeout to two seconds and verifies that Retry-After: 60 causes only one request, returns 504 to the client, and does not publish a task root.

The case verifies the default closure depth of 64: 64 validated child nodes retain checkpoints, while an over-depth root is not published and its first out-of-range child is not requested. A separate controlled low-threshold run sets the object limit to 3 and closure byte limit to 1. It verifies permanent closure_limit failure, no root publication, and no root checkpoint. Default Helm rendering leaves these environment variables unset. Go configuration unit tests verify defaults of 10,000 objects, 1 TiB of closure bytes, depth 64, and a 300-second request timeout. Invalid upstream descriptor.data Base64, size, digest, and media type each return 502 without publishing a solidified root. Client Range requests are served from a complete upstream GET that does not send a Range header.

For failed feature scenarios, use environment inspect or destroy with the run ID. The run record recovers the scenario and release from that ID.

See the [environment guide](environment/README.md) for prerequisites and environment management.

## Registered Cases

| ID | Coverage | Dependencies |
| --- | --- | --- |
| default-render | Deterministic empty-values rendering and paired external-auth validation | None |
| installation | Initialization Job, default PVCs, Deployment, backend digest, and client Service | default-render |
| initialization-secrets | Internal Registry authentication, TLS, and initialization records | installation |
| initializer-faults | Production initialization command concurrency, errors, certificate checks, and recovery in an isolated Kubernetes API simulator | installation |
| admin-auth | Admin authentication Secret, permissions, and authenticated documentation endpoint | installation |
| rbac-boundary | Initialization Role scope, separation of workload identities, and Registry token settings | installation |
| startup-wait | Init container blocks startup while initialization is pending; Deployment becomes Ready after recovery | installation |
| registry-roundtrip | OCI content upload/read and digest consistency | admin-auth, initialization-secrets |
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
| admin-api | Admin authentication, tasks, reconcile, disabled GC, and documentation endpoint | security-defaults, referrers-read |
| artifact-persistence | Persistence of content, referrers, failed tasks, Secrets, records, and PVCs across restart/upgrade | artifact-roundtrip, referrers-read, tag-publication, admin-api |
| dependency-failure | Distribution scale-down, diagnostic Service, backend errors, and recovery | artifact-persistence |
| default-limits | Default request-rate limits for manifests, descriptors, uploads, tags/list, and unauthenticated requests | manifest-validation, tags-pagination |
| observability | JSON logs, Prometheus metrics, low-cardinality labels, and sensitive-data scans | dependency-failure, default-limits, referrers-lifecycle, delete-semantics |
| podman-upstream-pull | Podman pulls a pinned image from the NJU GHCR mirror by tag, index digest, and platform manifest digest | installation |
| upstream-mirror | Local-first behavior, Docker/OCI media negotiation, upstream error matrix, descriptor.data/urls, nested multi-platform index graphs, original-field solidification, non-recursive subjects, HEAD metadata/cache boundaries, shared digest conflicts, default closure depth, controlled closure/object-byte limits, Range/If-Range, and digest validation | installation |
