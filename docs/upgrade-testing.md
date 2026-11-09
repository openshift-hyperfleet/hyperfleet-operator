# OLM Classic upgrade testing

The `test/e2e/upgrade` suite implements the operator-side N−1 → N checks for
HYPERFLEET-1413. It installs published bundles through OLM Classic on a prepared,
isolated cluster. It does not build the operator or install this checkout's CRDs.
The existing `make test-e2e` suite continues to test a locally built manager.

## Release and environment contract

Before running, qualify these inputs together:

- N−1 is an official final operator release. N is a newer RC in the same major
  version. Their API image digests must differ; an operator-only release cannot
  prove the API image transition required by 1413.
- One digest-pinned catalog contains both digest-pinned bundles and a direct
  `replaces` edge from N to N−1 in the selected channel. N is the channel head.
  This suite supports explicit sequential graphs, not `skips`/`skipRange` paths.
- Both bundles support `AllNamespaces` and own only the HyperFleetConfig CRD.
  Bundle CSVs, related images, and `RELATED_IMAGE_*` values must agree.
- A scenario names the components actually shipped by each release, their
  Deployment/container names, whether that release publishes `status.components`,
  and whether each surviving component's configuration hash should change.
  The suite also checks the actual operator-owned Deployment inventory.
- Fixtures must work with both versions. They are namespaced, created rather
  than applied over existing resources, and their images are pinned by digest.
  Keep the database, broker, authentication and TLS settings stable across the
  upgrade. The operator performs its own migrations.
- The cluster has OLM Classic installed, working registry access, sufficient
  capacity, and Kubernetes/CPU architectures supported by both releases. OLM v1
  is outside this suite's scope.

Use a freshly allocated, exclusively reserved cluster. Namespace isolation is
insufficient because `HyperFleetConfig/cluster` and its CRD are cluster-scoped.
The suite refuses an existing HyperFleetConfig CRD or HyperFleet Subscription,
creates its own namespace, and takes a fixed Lease in `kube-system`. An existing
Lease is never stolen based on elapsed time.

The kubeconfig needs cluster-admin-equivalent test privileges: read nodes/version;
manage the run namespace, fixtures, OLM resources, HyperFleetConfig and its CRD;
create/delete the test Lease; read logs; and port-forward to the API. This is a
test-runner permission requirement, not a change to operator RBAC.

## Inputs

| Input | Meaning |
| --- | --- |
| `KUBECONFIG` | Required explicit kubeconfig file for the isolated cluster. No ambient default is used. |
| `UPGRADE_FROM_BUNDLE` | Required previous-final bundle image by digest. |
| `UPGRADE_TO_BUNDLE` | Required target-RC bundle image by digest. |
| `UPGRADE_CATALOG` | Required aggregate catalog image by digest. |
| `UPGRADE_CHANNEL` | Required exact channel name; no hardcoded `stable` assumption. |
| `UPGRADE_SCENARIO` | Required YAML scenario; relative file paths resolve beside this file. |
| `ARTIFACT_DIR` | Required unique directory for one run, including `cleanup.json` and JUnit output. |
| `UPGRADE_NAMESPACE` | Fresh run namespace; default `hyperfleet-upgrade`. |
| `UPGRADE_TIMEOUT` | Deadline for an individual convergence stage; default `15m`. |
| `OPM` | Path to the installed `opm` executable; default `opm` on PATH. |
| `SKOPEO` | Path to `skopeo` for registry/platform preflight; default `skopeo` on PATH. |
| `UPGRADE_RETAIN_ON_SUCCESS` | Default `false`. Retain only a successful installation for downstream journeys. |

Use the repository's pinned Go version. The Make target obtains the repository's
pinned `opm` in `bin/` if absent; override `OPM` to use an existing executable.
Direct Go invocation defaults to `opm` on PATH. Install `skopeo` in the runner
and pin the runner/tool revision in CI. `opm render` must emit the
unmigrated `olm.bundle.object` properties for bundle CSV inspection. The suite
uses the installed CLI's normal registry authentication, runs `opm validate`,
and derives CSV identities and expected images from the immutable bundle images.
The runner also needs a configured containers/image signature policy accepted by
that `opm` build; the harness does not change the host's trust policy.

Before creating cluster resources, `skopeo inspect --config` verifies that the
catalog, operator, related operand images and fixture images are readable and
provide every schedulable node's Linux architecture. Both CLIs need registry
authentication configured in their normal credential files. Bundle images are
metadata, not executable workloads; their content is checked through `opm`.
Registry inspection does not replace the subsequent cluster image-pull checks.

## Scenario and fixtures

Start with
[`scenario.example.yaml`](../test/e2e/upgrade/testdata/scenario.example.yaml) and
[`fixtures.example.yaml`](../test/e2e/upgrade/testdata/fixtures.example.yaml).
These are templates, not a qualified release pair. The example illustrates an
API-only previous release upgrading to API plus five Sentinels; edit both
inventories to match the selected artifacts. The API container is named
`hyperfleet-api`; Sentinel containers are named `sentinel` in current releases.

Substitute verified PostgreSQL/RabbitMQ image digests and randomly generated
fixture passwords before use. The example broker password must be URL-safe
(for example, random hex). Keep rendered fixture files outside the repository
and protect them with mode `0600`. Do not put registry credentials in YAML.

`hashChange` is a required boolean for every target component also present in
N−1. Determine it from the selected releases' rendered configuration and hash
semantics. Image changes alone do not imply a config-hash change. This assertion
is separate from the metadata-only database Secret probe, which must change the
API hash and roll the API without rolling unrelated Sentinels.

`maxUnavailable` is required. Existing components must remain available in every
observed Deployment update and, when published, their per-component conditions.
Set `0s` when the component inventory is unchanged. A positive value bounds only
an aggregate HFC availability dip attributed to explicitly added components;
their conditions must identify the cause and all must recover. It never permits
an existing API or Sentinel outage. This does not establish a zero-downtime
traffic guarantee. Missing/expired watch history fails the test.

`settleDuration` defaults to `15s` (minimum `5s`). It bounds the no-op annotation
and post-Secret-update stability observations. These checks do not claim that
an unexpected rollout can never happen after the observation window.

The data probe creates a unique, non-sensitive API record before upgrade and
reads it through a **new** port-forward after upgrade. It compares the record's
ID, name, spec and labels, excluding mutable status/timestamps, then requests
deletion and waits for GET to return HTTP 404. `dataProbe.body` must be valid for
both API versions; its `name` is replaced with the run's unique name and its
labels receive an `upgrade_run` ownership marker. Both versions must support
listing this collection with a `search` filter on `name` and `labels.upgrade_run`;
this recovers the record if POST commits but its response is lost.
`port` is the API **pod** port, not an
arbitrary Service port. The example uses the Channel collection, whose contract
must be checked at the selected versions.

The example disables API authentication and uses HTTP only on the isolated test
cluster. For authenticated/TLS coverage, supply compatible fixture Secrets and
HFC settings, a `dataProbe.tokenFile`, and both `caFile` and `serverName` for TLS.
TLS verification is never disabled, and HTTP redirects are rejected. Keep the
token file valid through cleanup as well as the test.

## Execution

After preparing a scenario, fixtures, OLM and the real release artifacts:

```sh
GOTOOLCHAIN=go1.26.0 make test-e2e-upgrade \
  KUBECONFIG="$UPGRADE_KUBECONFIG" \
  UPGRADE_FROM_BUNDLE="$PREVIOUS_BUNDLE_DIGEST" \
  UPGRADE_TO_BUNDLE="$RC_BUNDLE_DIGEST" \
  UPGRADE_CATALOG="$RC_CATALOG_DIGEST" \
  UPGRADE_CHANNEL="$RC_CHANNEL" \
  UPGRADE_SCENARIO="$SCENARIO_FILE" \
  ARTIFACT_DIR="$RUN_ARTIFACT_DIR"
```

Missing prerequisites fail this target. The live Ginkgo entry point is guarded
by the `upgrade_e2e` build tag; ordinary unit tests never silently attempt a
cluster connection. The Go test process has a two-hour ceiling, so keep stage
timeouts consistent with that ceiling and the outer CI job timeout.

The test controls both approvals: N−1's initial InstallPlan, then N's upgrade
InstallPlan. It checks the exact CSV, Subscription owner, referenced plan UID
and namespace, and unchanged catalog source, and
uses a resource-version/UID-checked patch. It does not patch operand images,
replace the CatalogSource, or pre-run database migrations.

The target asserts:

1. CSV success and Subscription `installedCSV`, plus operator image/rollout health.
2. The original HFC survives; its expected components reach their release images,
   current status generation and completed rollouts. No old live pods may remain.
3. The target API's migration init container uses the target image and exits zero.
4. A complete `Progressing=False → True → False` history, continuous reported
   availability of existing workloads, and bounded startup of added components.
5. Release-pair hash expectations, API data survival, a no-op HFC annotation,
   and a metadata-only Secret change with correctly scoped rollout effects.

## Evidence and cleanup

Artifacts include the selected image/CSV identities, Go/opm/skopeo versions and
platform checks, condition and Deployment/pod watch histories, before/after
workload generations and hashes, API record fingerprint, OLM summaries,
pod/runtime-image/migration status, cluster version, bounded redacted logs,
result and JUnit reports. Secret objects, configuration payloads and arbitrary
condition messages are not exported. Logs are scrubbed against namespace Secret
values, the probe token, bearer tokens and JWTs. Restrict artifact access as well.
CI must also archive the runner image digest and test checkout revision, with
any local patch when testing an uncommitted checkout, alongside these artifacts.

Cleanup uses a separate bounded context even when the test fails or is cancelled.
It confirms deletion of the data probe, deletes the original HFC and waits for
foreground deletion, removes the Subscription and its InstallPlans before the
CSVs, removes the test-introduced CRD, deletes the namespace, and releases the
Lease. Probe recovery and deletion have a two-minute ceiling (also bounded by
the stage timeout and half the remaining cleanup time), reserving time for
Kubernetes teardown. An accepted DELETE alone does not prove finalization.
A failed data-probe deletion does not prevent Kubernetes teardown; its
receipt remains for the owner to resolve any record in an external database.
Deletions use UID
preconditions. Pre-existing/replacement resources are not adopted. A failed
cleanup remains a test failure and leaves the Lease/receipt for investigation.

For a composed RC workflow, set `UPGRADE_RETAIN_ON_SUCCESS=true`. The outer
workflow must run this in its **always-run post step**, after downstream tests:

```sh
GOTOOLCHAIN=go1.26.0 make cleanup-e2e-upgrade \
  KUBECONFIG="$UPGRADE_KUBECONFIG" ARTIFACT_DIR="$RUN_ARTIFACT_DIR"
```

The receipt binds cleanup to the cluster's `kube-system` UID and the run's
namespace/Lease/Subscription/HFC/CRD identities. Preserve it if a process is force-killed.
Lease, namespace and API record creation intents are saved before their requests.
After a lost response, cleanup recovers only resources bearing this run's
ownership markers, then records their identities before deleting them. A missing,
ambiguous or foreign resource cannot clear a pending creation receipt; cleanup
fails and preserves the receipt for investigation. API records are never
force-deleted to bypass finalization.
A kill or unreachable API can prevent cleanup; the outer owner must still
destroy the disposable cluster and handle any external database probe record.
The suite does not provision or destroy the cluster, install/uninstall OLM, or
modify the real release pipelines.

## Local verification and CI integration

```sh
GOTOOLCHAIN=go1.26.0 go test -race ./test/e2e/upgrade
GOTOOLCHAIN=go1.26.0 go test -tags=upgrade_e2e ./test/e2e/upgrade -run '^$'
GOTOOLCHAIN=go1.26.0 make lint-config lint test build
git diff --check
```

`make test` includes the upgrade harness's ordinary unit tests while excluding
the legacy live `test/e2e` package. Compiling the tagged entry point does not
prove a live upgrade.

Prow wiring belongs in `openshift/release`; RC inputs/result enforcement belong
in `hyperfleet-release`. Shared E2E journeys need an operator-compatible setup
before they can use a retained installation. Catalog publishing is separately
tracked by HYPERFLEET-1884/1770/1773. HYPERFLEET-1868 records placement and cadence:
the current 1413 Jira requirement says every relevant change, while the RC-only
proposal needs an agreed requirement update. This repository target does not
claim either CI cadence is already configured.

The checked-out bundle tag pipeline already derives RC/final CSV versions from
`bundle-v*` tags, but still emits `candidate-vX,stable-vX` bundle annotations.
Architecture PR #259 proposes `stable-vX` metadata and a separate aggregate
release catalog. That release work remains separately owned; this test treats
the supplied catalog's selected channel as authoritative and never rewrites it.
