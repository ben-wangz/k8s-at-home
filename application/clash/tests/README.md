# Clash Console regression tests

This suite owns its runner, metadata, fixtures, browser dependencies, and run state. It has no dependency on Registry Hub files, packages, scripts, or deployments. Product code remains under `console/frontend/`; test fixture servers are used only during development and regression runs.

## Preparation

Use Go matching `go.mod`, Node.js 24 or a compatible version, npm, and Helm. Cluster scenarios additionally require kubectl, Podman when images need building or digest resolution, and an existing test namespace. Install dependencies and build the UI using absolute paths:

```bash
CLASH_APP=/absolute/path/to/application/clash
npm --prefix "$CLASH_APP/console/frontend" ci
npm --prefix "$CLASH_APP/console/frontend" run build
npm --prefix "$CLASH_APP/tests/browser" ci
npm --prefix "$CLASH_APP/tests/browser" run install-browser
helm dependency update "$CLASH_APP/chart"
```

Set `CLASH_TEST_BROWSER_EXECUTABLE` to an existing compatible Chrome/Chromium executable when using a browser installed by the environment. Otherwise Playwright uses its pinned browser installation. Browser and runner dependencies are excluded from the chart and product artifacts.

## Local checks

Run browser workflows against the independent mock controller:

```bash
npm --prefix "$CLASH_APP/tests/browser" test
```

The fixture serves the compiled chart assets, REST APIs, and WebSocket streams. Tests cover errors, optional metadata, buffer limits, and stream lifecycle without a personal subscription. These results are labeled as mock evidence.

Run the full minimal plan with mock browser execution and aggregate reports:

```bash
"$CLASH_APP/tests/features/minimal/run.sh" run --mock
"$CLASH_APP/tests/features/minimal/run.sh" run --mock --scenario feature-controller-auth
"$CLASH_APP/tests/features/minimal/run.sh" run --mock --scenario feature-controller-failure
go -C "$CLASH_APP/tests" test ./...
```

Mock mode does not prove real Mihomo or ingress compatibility. The independent-checkout case copies only Clash source into a temporary checkout, then builds, lists, plans, and checks the runner there.

## Plan and select cases

```bash
"$CLASH_APP/tests/features/minimal/run.sh" list
"$CLASH_APP/tests/features/minimal/run.sh" plan --case proxy-selection
"$CLASH_APP/tests/features/minimal/run.sh" run --case proxy-selection --case connections
```

`list` and `plan` require no cluster access or container builds. Case selection expands dependencies and is deterministic. Cases and browser workers run serially in each environment. Each browser case uses a fresh browser context. Metadata under `features/minimal/cases/` declares timeouts, preconditions, requirements, checks, expected evidence, and browser specs. The runner rejects passing checks whose evidence IDs or artifact files do not satisfy their declarations. `coverage.json` tracks acceptance status and verification modes. Report paths are relative to `${XDG_STATE_HOME:-$HOME/.local/state}/clash/tests/`.

## Real integration scenarios

Set the Kubernetes context and existing namespace explicitly. The default namespace is `develop`; the current kubeconfig context is used when no context override is provided.

```bash
export CLASH_TEST_CONTEXT=develop
export CLASH_TEST_NAMESPACE=develop
export CLASH_TEST_IMAGE_REPOSITORY=registry.example.com/clash-tests/fixture
"$CLASH_APP/tests/features/minimal/run.sh" check
"$CLASH_APP/tests/features/minimal/run.sh" run
"$CLASH_APP/tests/features/minimal/run.sh" run --scenario feature-controller-auth
"$CLASH_APP/tests/features/minimal/run.sh" run --scenario feature-controller-failure
```

The runner uses Podman to build a small fixture image and push it to the configured repository. Authentication must be available to Podman, and cluster nodes must be able to pull the result. `CLASH_TEST_FIXTURE_IMAGE=<repository>@sha256:<digest>` reuses a prebuilt fixture. The fixture exposes two HTTP proxy nodes, a local subscription, and a bounded-rate streaming destination. Core checks require no public proxy subscription or external traffic destination.

Mihomo defaults to the version in the chart. Set `CLASH_TEST_MIHOMO_IMAGE` to a reachable mirror reference when needed. Mutable image references are resolved to immutable digests before deployment. A digest reference avoids the corresponding Podman pull. The report records both image identities and the version returned by the running controller.

Real runs install a uniquely named Clash release and fixtures. The workstation and browser must reach this run's controller Service IP directly. Traffic generation sends an absolute URL through Mihomo's mixed proxy port; only Mihomo needs to resolve the fixture's cluster DNS name. This avoids port forwarding and changes to existing applications.

Scenarios:

| ID | Scope |
| --- | --- |
| `feature-minimal` | Static UI, proxy inventory and selection, traffic, connections, logs, and deterministic mock fault cases. |
| `feature-controller-auth` | A generated controller Secret, rejected credentials, and authenticated HTTP and streaming APIs. |
| `feature-controller-failure` | Scale this run's Mihomo Deployment down and back up; verify stale data and recovery. |
| `feature-ingress` | Optional environment smoke test for HTTPS `/ui/`, reload, authenticated APIs, and WebSockets through an existing ingress controller. |

To run the optional ingress smoke test, provide `CLASH_TEST_INGRESS_HOST`, `CLASH_TEST_TLS_CERT_FILE`, and `CLASH_TEST_TLS_KEY_FILE`. DNS must route that hostname to the ingress controller, and the certificate must be trusted by the workstation and browser. `CLASH_TEST_INGRESS_CLASS` defaults to `nginx`. TLS data is copied into a run-owned Secret. Missing prerequisites fail explicitly; they are not silently skipped. This test is not required for CC-007, which covers controller Service access and chart ingress rendering; live routing and TLS termination belong to the Kubernetes environment.

## Environment lifecycle and evidence

```bash
"$CLASH_APP/tests/features/minimal/run.sh" environment create --id console-check
"$CLASH_APP/tests/features/minimal/run.sh" environment inspect --id console-check
"$CLASH_APP/tests/features/minimal/run.sh" run --id console-check --reuse --case proxy-selection
"$CLASH_APP/tests/features/minimal/run.sh" environment destroy --id console-check
```

The runner prints a generated ID when omitted. IDs start with a lowercase letter and contain up to 32 lowercase letters, digits, or hyphens. A new run cannot overwrite an existing ID. Explicit reuse validates scenario, source, configuration, context, namespace, and resource identities; changed source requires a new ID.

Successful test runs remove their owned Kubernetes resources, including Helm release storage. Environment-only creation leaves them available. Failures and interruptions retain resources for inspection. Cleanup uses ownership labels and UID deletion preconditions, never deleting cluster-scoped resources. A local lock and a renewable namespace Lease prevent competing mutations for the same run.

Real minimal runs exercise the lifecycle commands using a second run-owned environment: create and inspect it, force an installation check to fail, inspect the preserved failure, restore the workload, reuse the environment successfully, and explicitly destroy it. Mock runs execute the local lifecycle unit checks but do not count as integration evidence for these commands.

Reports live under `${XDG_STATE_HOME:-$HOME/.local/state}/clash/tests/<scenario>/<id>/`. Caches and temporary runner binaries use `${XDG_CACHE_HOME:-$HOME/.cache}/clash/tests/`. Each declared passing check needs nonempty evidence. Browser JSON reports contain assertion results, screenshots, and structured evidence; mock and integration modes remain distinguishable. Authentication secrets are stored in a private local file for the browser process and are excluded from reports and screenshots. Raw browser traces and network archives are disabled.

Assertion failures and cleanup failures have separate states. Run-scoped workload diagnostics supplement controller log evidence; Kubernetes Pod logs cannot replace `/logs` assertions. Optional cross-origin deployments require additional environment-specific validation.
