# OLM Classic upgrade testing

The `test/e2e/upgrade` suite implements the operator-side N−1 → N checks for
HYPERFLEET-1413. It installs published bundles through OLM Classic on a prepared,
isolated cluster. It does not build the operator or install this checkout's CRDs.
The existing `make test-e2e` suite continues to test a locally built manager.

## Operand upgrade policy

The installed operator reconciles the same `HyperFleetConfig/cluster` to the
operand images and configuration shipped by **that operator release**. The CSV's
`spec.relatedImages` and `RELATED_IMAGE_*` environment variables identify those
images. Operand version numbers need not equal the operator version number;
partners do not select an independent operand version through the CR.

This suite validates sequential upgrades only: N must directly replace N−1 in
the selected catalog channel. Downgrades and skipped release edges are outside
the supported test contract. The original CR and its intent survive the upgrade;
the new operator reconciles its workloads and runs its shipped API migrations.
See [ADR-0019](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0019-package-hyperfleet-as-operator.md)
for the ownership boundary and [the component image contract](component-pattern.md#8-add-the-image-and-related-image-plumbing).

## Release and environment contract

Before running, qualify these inputs together:

- **Release pair:** N−1 is an official final release; N is a newer RC in the same major version.
- **API transition:** Their API image digests differ so the run can prove an API
  upgrade. A Sentinel image need not differ: N−1 may not have Sentinels, and any
  Sentinel present in either release is checked against that release's CSV.
- **Catalog:** One catalog image, pinned by digest for a repeatable run, contains
  both digest-pinned bundles and remains unchanged throughout the test.
- **Upgrade edge:** In one selected channel, N directly `replaces` N−1 and is
  the channel head; no `skips`/`skipRange` path. The Subscription starts at
  N−1's CSV and then upgrades to N through that same catalog and channel.
- **Install mode:** Both bundles support `AllNamespaces` and own only the HyperFleetConfig CRD.
- **Image contract:** CSV related images and `RELATED_IMAGE_*` values agree.
- **Inventory:** The scenario describes each release's actual components and Deployment/container names.
- **Status contract:** The scenario declares whether each release publishes `status.components`.
- **Hash contract:** Each surviving component has an explicit expected hash-change decision.
- **Exact target hash:** The scenario binds a supported hash scheme and every target component's ordered ConfigMap keys and Secret roles to the exact target bundle digest. Qualify these inputs against that bundle's source.
- **Fixtures:** Namespaced fixtures work with both versions and use digest-pinned images.
- **Stable dependencies:** Keep database, broker, authentication and TLS settings stable during upgrade.
- **Migration ownership:** The operator performs its own migrations.
- **Cluster:** OLM Classic, registry access and sufficient capacity are available on a supported Kubernetes/CPU platform.

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
| `OLM_CATALOG_IMAGE` | Required aggregate catalog image by digest, shared by both releases. |
| `OLM_CHANNEL` | Required OLM Subscription channel used for both releases; no hardcoded `stable` assumption. |
| `UPGRADE_SCENARIO` | Required YAML scenario; relative file paths resolve beside this file. |
| `ARTIFACT_DIR` | Required unique directory for one run, including `cleanup.json` and JUnit output. |
| `UPGRADE_NAMESPACE` | Fresh run namespace; default `hyperfleet-upgrade`. |
| `UPGRADE_TIMEOUT` | Deadline for an individual convergence stage; default `15m`. |
| `OPM` | Path to the installed `opm` executable; default `opm` on PATH. |
| `PODMAN` | Path to `podman` for registry/platform preflight; default `podman` on PATH. |
| `UPGRADE_RETAIN_ON_SUCCESS` | Default `false`. Retain only a successful installation for downstream journeys. |

When using the Make targets, relative `KUBECONFIG`, `UPGRADE_SCENARIO` and
`ARTIFACT_DIR` paths are resolved from the repository root before Go starts the
package test. Paths within the scenario remain relative to the scenario file.

Use the repository's pinned Go version. The Make target obtains the repository's
pinned `opm` in `bin/` if absent; override `OPM` to use an existing executable.
Direct Go invocation defaults to `opm` on PATH. Install Podman in the runner
and pin the runner/tool revision in CI. Podman needs working image storage and
registry access; a Linux CI pod must be configured for nested Podman. Installing
the binary alone is insufficient. A remote Podman client needs a reachable
service (on macOS, typically a running Podman machine). The suite checks the CLI
version first during platform preflight and fails before cluster mutation if
the executable is missing or image pull/inspection fails. `opm render` must emit the
unmigrated `olm.bundle.object` properties for bundle CSV inspection. The suite
uses the installed CLI's normal registry authentication, runs `opm validate`,
and derives CSV identities and expected images from the immutable bundle images.
The runner also needs a configured containers/image signature policy accepted by
that `opm` build; the harness does not change the host's trust policy.

Before creating cluster resources, the suite runs `podman pull --quiet
--policy=always --platform <os>/<arch> <image>` for the catalog, operator,
related operand images and fixture images on every schedulable node platform.
It then runs `podman image inspect --format '{{.Os}}/{{.Architecture}}' <image-id>`
using the full ID returned by that pull and compares the actual OS/architecture.
Inspecting by ID avoids ambiguity when multiple platforms share a registry
reference. Both CLIs need registry authentication configured in their normal
credential files. Bundle images are metadata, not executable workloads; their
content is checked through `opm`.

This uses Podman in place of a separate Skopeo dependency, at the cost of pulling
image layers into the runner's Podman storage. Provision sufficient disk and
stage timeout for those pulls. The suite leaves cached images in place; use
disposable runner storage or manage it outside the test, without pruning images
used by other jobs. These pulls do not populate the target cluster nodes' image
caches or replace the subsequent cluster image-pull checks.

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

Replace every literal placeholder before loading the files; the harness does
not expand environment variables in YAML:

| Placeholder | Required replacement |
| --- | --- |
| `${UPGRADE_POSTGRES_IMAGE}` | Full PostgreSQL image reference with a verified `@sha256:` digest, compatible with both releases and the fixture's UID/security settings. |
| `${UPGRADE_RABBITMQ_IMAGE}` | Full RabbitMQ image reference with a verified `@sha256:` digest and compatible fixture settings. |
| `${UPGRADE_DB_PASSWORD}` | Random database fixture password; the PostgreSQL pod reads the same Secret. |
| `${UPGRADE_BROKER_PASSWORD}` | Random URL-safe broker fixture password; use the same value in both Secret entries. |
| `${UPGRADE_API_HASH_CHANGE}` | Unquoted YAML `true` or `false`, determined from the selected release pair. |
| `${UPGRADE_TO_BUNDLE_DIGEST}` | The same full digest-pinned bundle reference passed as `UPGRADE_TO_BUNDLE`, after verifying its source uses the declared hash scheme and inputs. |

`hashChange` is a required boolean for every target component also present in
N−1. It checks whether that component's config-hash annotation should change
between the two releases. For example, if only the API image changes while its
configuration inputs and hash scheme stay the same, set `hashChange: false`;
if a hashed input changes, set it to `true`. Determine the value from the
selected releases' configuration and hash semantics. The exact target hash
check below asks a different question: whether N's annotation matches N's
applied inputs. The later metadata-only database Secret probe must change the
API hash and roll the API without rolling unrelated Sentinels.

For the target release, `to.hashContract.bundle` must equal the immutable
`UPGRADE_TO_BUNDLE` reference. The supported test-only scheme is
`config-sha256-v1`. For **every** target component, list `hashInputs.configMaps`
in the release's `Rollout().ConfigEntries` order and list each
`ReferencedSecrets(...)` role and name under `hashInputs.secrets`. For this
example the API hashes one `config.yaml` and the database Secret metadata;
each Sentinel hashes `config.yaml`, `broker.yaml`, and the broker Secret
metadata. Verify these declarations against the source commit that built the
target bundle. The example is not evidence that any published RC uses this
contract. A release with another scheme needs a separately qualified oracle;
the suite fails closed instead of importing this checkout's controller code.

Once N is healthy, the suite independently calculates the SHA-256 hash from
the **applied** ConfigMap values and referenced Secret resourceVersions, then
compares it with every target Deployment's config-hash annotation. It repeats
the exact API check after the database Secret metadata probe. The exact-hash
reader requests only Secret metadata; the recorded exact-hash evidence
contains only source identities and metadata versions, expected digests, and
observed digests. This proves that the annotation matches the applied inputs,
not that the rendered configuration itself is correct. The existing
`hashChange` checks still assert the expected behavior across releases. The
current algorithm's own properties are also tested in
[`TestComputeConfigHashProperties`](../internal/controller/hyperfleetconfig_rollout_test.go).

- **Availability budget:** `maxUnavailable` is required; use `0s` for unchanged inventory.
- **Existing components:** Every observed Deployment update and published component condition must remain available.
- **Added components:** A positive budget permits only an aggregate HFC dip attributed to their startup; all must recover.
- **Traffic availability:** Readiness, status and workload history show whether
  Kubernetes reported the API available during the observed upgrade. They do
  not prove that every client request succeeded or that there was no
  request-level downtime.
- **Watch recovery:** An HFC watch expiry (`410 Expired/Gone`), including after
  an OLM CRD schema update, triggers a fresh singleton LIST and resumes watching
  from the collection's resourceVersion. The HFC's own resourceVersion may still
  predate the rebuilt watch cache. Each recovery appends a sanitized observation
  with a `resync` marker containing the old and new watch checkpoints in
  `conditions.json`; earlier observations remain intact. Missing, replaced,
  deleting or spec-changed HFCs and other watch errors fail the test.
- **Evidence limits:** A resync cannot recover missed condition updates or prove
  continuous HFC/component conditions across that gap. The observed
  `Progressing=False → True → False` sequence is still required; a healthy resync
  does not substitute for a missed `True`, or erase an observed availability
  failure. Deployment and pod histories remain strict: expiry or lost history
  fails the test, preserving the independent workload-availability check.

`settleDuration` defaults to `15s` (minimum `5s`). It bounds the no-op annotation
and post-Secret-update stability observations. These checks do not claim that
an unexpected rollout can never happen after the observation window.

- **Data survival:** Create a unique non-sensitive record before upgrade and read it through a new port-forward afterward.
- **Comparison:** Preserve ID, name, spec and labels; ignore mutable status/timestamps.
- **Deletion:** Request deletion and wait for GET to return HTTP 404.
- **Request body:** `dataProbe.body` must work in both API versions; the harness supplies a unique name and `upgrade_run` label.
- **Lost-response recovery:** Both versions must support collection search by `name` and `labels.upgrade_run`.
- **Pod port:** `dataProbe.port` selects the API pod's listening port.
- **Collection:** The example uses Channels; verify its contract in both selected versions.

The example disables API authentication and uses HTTP only on the isolated test
cluster. For authenticated/TLS coverage, supply compatible fixture Secrets and
HFC settings, a `dataProbe.tokenFile`, and both `caFile` and `serverName` for TLS.
TLS verification is never disabled, and HTTP redirects are rejected. Keep the
token file valid through cleanup as well as the test.

## Execution

After preparing a scenario, fixtures, OLM and the real release artifacts:

```sh
make test-e2e-upgrade \
  KUBECONFIG="$UPGRADE_KUBECONFIG" \
  UPGRADE_FROM_BUNDLE="$PREVIOUS_BUNDLE_DIGEST" \
  UPGRADE_TO_BUNDLE="$RC_BUNDLE_DIGEST" \
  OLM_CATALOG_IMAGE="$RC_CATALOG_DIGEST" \
  OLM_CHANNEL="$RC_CHANNEL" \
  UPGRADE_SCENARIO="$SCENARIO_FILE" \
  ARTIFACT_DIR="$RUN_ARTIFACT_DIR"
```

Make defaults to `GOTOOLCHAIN=go1.26.0`, matching `go.mod` and the pinned tools.
An explicit environment or Make command-line value overrides that default.
`OLM_CATALOG_IMAGE` selects the catalog containing both releases. `OLM_CHANNEL`
selects the channel containing the direct N−1 → N upgrade edge. The catalog
and channel remain unchanged throughout the test.

Missing prerequisites fail this target. The live Ginkgo entry point is guarded
by the `upgrade_e2e` build tag; ordinary unit tests never silently attempt a
cluster connection. The two-hour Ginkgo limit is a safety ceiling, not a
measured or expected runtime. It allows bounded stage waits and diagnostics;
up to 25 minutes remain for interrupted cleanup. Go allows 2 hours 30 minutes
for the process. Keep stage timeouts and the outer CI job timeout consistent
with those limits.

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
4. An observed `Progressing=False → True → False` sequence, continuous reported
   availability of existing workloads, and bounded startup of added components
   within the condition-history evidence limits above.
5. Release-pair hash expectations and exact target hashes for every component,
   API data survival, a no-op HFC annotation, and a metadata-only Secret change
   with an exact new API hash and correctly scoped rollout effects.

## Evidence and cleanup

Artifacts include the selected image/CSV identities, Go/opm/Podman versions and
platform checks, condition history with explicit resync markers and
Deployment/pod watch histories, before/after
workload generations and hashes, exact target hash comparisons and source
metadata identities, API record fingerprint, OLM summaries,
pod/runtime-image/migration status, cluster version, bounded redacted logs,
result and JUnit reports. Secret objects, configuration payloads and arbitrary
condition messages are not exported. Logs are scrubbed against namespace Secret
values, the probe token, bearer tokens and JWTs. Restrict artifact access as well.
Short or numeric Secret values are still redacted: their format does not prove
they are nonsensitive. This can obscure matching text in logs. Structured
condition/workload artifacts retain their allowlisted timestamps and counters
without applying free-text replacement. Encoded-value redaction covers common
base64, hex, JSON and URL forms, not arbitrary transformations of a credential.
CI must also archive the runner image digest and test checkout revision, with
any local patch when testing an uncommitted checkout, alongside these artifacts.

Cleanup uses a separate bounded context even when the test fails or is cancelled.
It confirms deletion of the data probe, deletes the original HFC with background
propagation and waits for the HFC to disappear, removes the Subscription and its
InstallPlans before the CSVs, removes the test-introduced CRD, waits for namespace
deletion, and releases the Lease. Background propagation avoids a foreground
deletion deadlock with published operators that still reconcile a terminating
HFC and recreate its dependents. HFC disappearance alone does not prove operand
cleanup: namespace deletion must also complete before the Lease is released.
Cleanup does not patch away custom finalizers. Probe recovery and deletion have
a two-minute ceiling (also bounded by the stage timeout and half the remaining
cleanup time), reserving time for Kubernetes teardown. An accepted DELETE alone
does not prove finalization.
A failed data-probe deletion does not prevent Kubernetes teardown; its
receipt remains for the owner to resolve any record in an external database.
Deletions use UID preconditions. CSV UIDs are saved when first observed. Before
each approval, the receipt also records the exact InstallPlan name/UID and a
filtered CSV LIST checkpoint proving that the selected CSV was absent. If an
approval response is lost before its CSV UID is recorded, cleanup first verifies
that the original approved plan belongs to the recorded Subscription, selects
the expected CSV/catalog, and reports the CSV step as `Created` (not `Present`).
It then replays the CSV watch from the saved checkpoint and accepts only the
original `Added` event whose UID matches the current CSV. The recovered UID is
saved before deleting the Subscription, InstallPlans or CSVs.

An approval flag, matching name, or namespace alone never establishes CSV
ownership. Missing provenance, expired creation history, or a replacement causes
cleanup to refuse deletion and retain the receipt/Lease for investigation. CSV
creation history cannot use the HFC watch's resync fallback: a fresh snapshot
cannot identify a previously unseen replacement. Older receipts without approval
checkpoints retain the same refusal behavior for unrecorded CSVs. Pre-existing
and replacement resources are not adopted. A failed cleanup remains a test
failure and leaves the Lease/receipt for investigation.

For a composed RC workflow, set `UPGRADE_RETAIN_ON_SUCCESS=true`. The outer
workflow must run this in its **always-run post step**, after downstream tests:

```sh
make cleanup-e2e-upgrade \
  KUBECONFIG="$UPGRADE_KUBECONFIG" ARTIFACT_DIR="$RUN_ARTIFACT_DIR"
```

The receipt binds cleanup to the cluster's `kube-system` UID and the run's
namespace/Lease/Subscription/HFC/CRD/CSV identities, approval-plan identities and
CSV creation checkpoints. Preserve it if a process is force-killed.
A receipt is saved before platform checks or cluster mutations. An early-failure
receipt with no cluster identity or pending mutations needs no cluster access to
clean up. A missing receipt is reported explicitly and cleanup makes no changes;
verify the artifact directory and run history rather than assuming no resources
exist. Standalone cleanup has a ten-minute total deadline across all steps.
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

The suite uses the official `github.com/operator-framework/api` OLM types for
CSV decoding, InstallPlan validation, and resource construction. Version
`v0.42.0` supports the repository's Go 1.26.0 toolchain; releases v0.43.0 through
v0.46.0 require Go 1.26.3. The dynamic client still performs cluster requests,
including the UID/resource-version-checked approval patch and cleanup deletes.

```sh
GOTOOLCHAIN=go1.26.0 go test -race ./test/e2e/upgrade
GOTOOLCHAIN=go1.26.0 go test -tags=upgrade_e2e ./test/e2e/upgrade -run '^$'
make lint-config lint test build
git diff --check
```

`make test` includes the upgrade harness's ordinary unit tests while excluding
the legacy live `test/e2e` package. Compiling the tagged entry point does not
prove a live upgrade. `make lint`, `make lint-fix` and `make vet` include the
`upgrade_e2e` build tag for static analysis without executing the live suite.

As of 2026-11-11, [HYPERFLEET-1413](https://redhat.atlassian.net/browse/HYPERFLEET-1413)
explicitly requires: "Test runs in CI on every relevant change." The operator's
[Prow configuration](https://github.com/openshift/release/blob/main/ci-operator/config/openshift-hyperfleet/hyperfleet-operator/openshift-hyperfleet-hyperfleet-operator-main.yaml)
runs lint and unit tests, and its [presubmit variant](https://github.com/openshift/release/blob/main/ci-operator/config/openshift-hyperfleet/hyperfleet-operator/openshift-hyperfleet-hyperfleet-operator-main__presubmits.yaml)
builds a bundle; neither runs this live upgrade target. The operator PR therefore
provides a runnable suite; the CI criterion remains open. Prow wiring belongs
in `openshift/release`; qualified RC inputs and result enforcement belong in
`hyperfleet-release`. The live job must run on every relevant change to meet
the current criterion. If a different cadence is proposed, obtain the story
owner's agreement and update 1413 before treating that criterion as complete;
tracking placement and cadence in
[HYPERFLEET-1868](https://redhat.atlassian.net/browse/HYPERFLEET-1868)
does not itself change 1413's acceptance criteria. Shared E2E journeys need an
operator-compatible setup before they can use a retained installation. Catalog
publishing is separately tracked by HYPERFLEET-1884/1770/1773.
The supplied catalog's selected channel is authoritative; this test never rewrites it.

The current [release manifest](https://github.com/openshift-hyperfleet/hyperfleet-release/blob/main/RELEASE_MANIFEST.yaml)
and [RC trigger](https://github.com/openshift-hyperfleet/hyperfleet-release/blob/main/scripts/trigger-rc-e2e.sh)
select component images and Helm chart refs; they do not supply an operator
bundle pair, catalog, or upgrade scenario. Their existing shared-GKE RC job
therefore cannot satisfy this criterion simply by adding the Make target.
Completing the CI integration requires:

1. Qualified immutable bundle/catalog references and a scenario/fixture set for
   that release pair, including the target hash contract.
2. A dedicated cluster (or equivalent full cluster isolation), OLM Classic,
   registry access, and a runner with Go, `opm`, and working nested Podman.
3. A job in `openshift/release` that invokes `make test-e2e-upgrade` for every
   relevant change, collects its JUnit and diagnostics even on failure, and
   cleans up the test installation and disposable infrastructure.
4. A successful run of that job, with the tested source revision and immutable
   release inputs recorded. A helper-test pass or manually triggered Helm E2E
   run does not close this acceptance criterion.
