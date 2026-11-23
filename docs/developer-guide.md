# Developer guide

This guide is for contributors changing the HyperFleet Operator. It covers the
repository, the reconciliation path, and the checks required before review. For
the public custom resource contract, use the
[`HyperFleetConfig` reference](hyperfleetconfig-reference.md). For the design
behind packaging HyperFleet as an operator, read
[ADR-0019](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0019-package-hyperfleet-as-operator.md).

Installation and release workflows are intentionally kept in the
[OLM guide](olm.md) and [disconnected-install guide](disconnected-install.md),
not duplicated here.

## Prerequisites

- Go 1.26.0, matching `go.mod`. Make defaults to `GOTOOLCHAIN=go1.26.0`, while honoring an explicit environment or command-line override. For direct `go` commands on a newer toolchain, use the same prefix; the pinned golangci-lint does not support Go 1.27.
- FIPS mode is enabled only in the manager container image. Local builds and
  tests do not enable it by default. The Dockerfile selects `GOFIPS140=v1.0.0`
  and builds the whole `./cmd` package with `-tags=fips,no_openssl` so that
  `cmd/fips.go` enables FIPS and rejects runtime attempts to disable it.
  `no_openssl` selects Go's native crypto backend on Red Hat Go. The image build
  checks the module, compiled FIPS default, and startup with `GOLANG_FIPS=1`.
  The related-images validator is build-time tooling, not a shipped service.

  Use `make test` for the non-E2E suite: it prepares `KUBEBUILDER_ASSETS` through
  `setup-envtest` and excludes `test/e2e`. A direct
  `go test ./...` does not perform that setup and also selects the E2E package.
  Run live E2E separately with `make test-e2e-kind` or `make test-e2e-ci` as
  described in [End-to-end tests](#end-to-end-tests).

  Do not set `fips140ems=0` through `GODEBUG`, `godebug` in `go.mod`/`go.work`,
  or `//go:debug` in the main package: it disables TLS 1.2 EMS enforcement.
  The Dockerfile rejects this compiled override even alongside `fips140=on`.
  Neither that build check nor the runtime FIPS-mode guard detects a
  deployment-time `GODEBUG=fips140ems=0` override; deployment configuration must
  be reviewed separately.
- Docker. The image and e2e targets are scaffolded and tested with Docker;
  alternative container tools are not assumed to work.
- `envsubst` (provided by gettext) for development OLM bundle overlays.
- `kubectl` and access to the cluster used for local runs.
- `kind` for end-to-end tests.
- [pre-commit](https://pre-commit.com/#install) for the repository git hooks.
- Network access when tools, envtest assets, base images, or cert-manager must be
  downloaded.

Repository tools such as `controller-gen`, `golangci-lint`, `kustomize`, and
`setup-envtest` are pinned in `tools/go.mod` and invoked through `go tool`; a
separate global installation is not required.

Run `make install-hooks` once after cloning to install the git hooks (LeakTK
secret scanning, commit message validation, Go tooling checks, and file
hygiene), configured in `.pre-commit-config.yaml`.

The current `.devcontainer/devcontainer.json` still selects Go 1.24 and does not
meet the `go.mod` requirement. Until that file is upgraded, do not treat the
devcontainer as a supported build environment for this branch.

## Contents

- [Prerequisites](#prerequisites)
- [Repository layout](#repository-layout)
- [Reconciliation model](#reconciliation-model)
- [Local development workflow](#local-development-workflow)
  - [Running the operator from the host](#running-the-operator-from-the-host)
  - [Running the operator in a cluster](#running-the-operator-in-a-cluster)
- [End-to-end tests](#end-to-end-tests)
- [Generated and release-facing files](#generated-and-release-facing-files)
- [Troubleshooting local development](#troubleshooting-local-development)

## Repository layout

| Path | Responsibility |
|---|---|
| `api/v1alpha1` | `HyperFleetConfig` and `ManagementClusterRegistration` APIs, validation markers, defaults, status vocabulary, and generated deep-copy code. |
| `cmd` | Manager startup, scheme registration, flags, related-image input, and controller wiring. |
| `internal/controller` | Reconciliation, watches, Secret resolution, server-side apply orchestration, rollout hashing, status rollup, and metrics. |
| `internal/bundle` | Resolves `spec.bundle` into an ordered set of components. |
| `internal/component` | Component implementations. Each renders desired objects and derives health from the applied objects. |
| `internal/apply` | Owner-reference handling and server-side apply. |
| `config/crd` | Generated CRD manifests. Do not hand-edit generated bases. |
| `config/rbac` | Generated RBAC plus role aggregation and namespace-scoped operand access. |
| `config/manager` | Base manager Deployment and image substitutions. |
| `config/manifests` | OLM CSV bases and development/production overlays. |
| `config/samples` | Example installation configuration and management-cluster registration. |
| `test/e2e` | Kind-based end-to-end setup and Ginkgo suite. |
| `validators` | Bundle validation, including digest-pinned related images. |
| `hack` | Build, validation, and disconnected-mirroring helpers. |
| `tools` | Pinned Go tool dependencies used by the Makefile. |

## Reconciliation model

There is one controller for the singleton `HyperFleetConfig`; components do not
get independent controllers. At a high level, one reconciliation:

1. Reads `HyperFleetConfig/cluster` and establishes the operator namespace.
2. Resolves referenced Secrets.
3. Calls `bundle.Resolve` to obtain shared components followed by
   bundle-specific components.
4. Calls each component's pure `Render` method.
5. Adds rollout hashes for rendered configuration and referenced Secrets.
6. Applies every object with server-side apply and the `HyperFleetConfig` as
   controller owner. The applied objects are refreshed in place with their
   current server status.
7. Passes those refreshed objects to each component's `Conditions` method and
   rolls component health into `Available`, `Progressing`, and `Degraded`.
8. Updates status and reconciliation metrics.

Read `internal/controller/hyperfleetconfig_controller.go` from top to bottom
before changing this flow. See the [component pattern](component-pattern.md)
before adding an operand.

## Local development workflow

Repository coding-agent guidance lives in [`AGENTS.md`](../AGENTS.md).

Start by confirming that generated files agree with the API and RBAC markers:

```sh
make manifests generate
git diff --check
```

Run static analysis and the full non-e2e suite:

```sh
make lint
make test
```

`make test` deliberately runs `manifests`, `generate`, `fmt`, `vet`, downloads
the matching envtest control-plane binaries, and then runs all Go packages
except `test/e2e` with coverage. It can therefore update generated or formatted
files. Always inspect `git diff` after it completes.

For a quick iteration on pure packages, run the relevant Go tests directly, for
example:

```sh
go test ./internal/bundle ./internal/component/api ./internal/apply
```

Controller tests require envtest. Use `make test` rather than assuming locally
installed Kubernetes control-plane binaries are compatible.

### Running the operator from the host

With a usable current kubeconfig:

```sh
make install
make run OPERATOR_NAMESPACE=hyperfleet-system
```

The namespace must exist and must contain every Secret referenced by the
cluster-scoped CR. `make run` executes the operator on the host; it does not
create the namespace, database, credentials, issuer, or certificates. Apply a
sample only after replacing its example values with reachable dependencies.

### Running the operator in a cluster

See [Component Install Tracker](component-install-tracker.md) for latest
requirements regarding installation of operator.

## End-to-end tests

### E2E - Kind

```sh
make test-e2e-kind KIND_CLUSTER=hyperfleet-operator-dev
```

The target creates the named Kind cluster only when it does not already exist. The suite requires KUBECONFIG to be set; kind writes the cluster context into the file it points at.
With `--kind-mode=true` the test suite handles building and loading the operator image (`make image`), installing the CRDs (`make install`), and deploying the operator manifests (`make deploy`).

`SKIP_CERT_MANAGER_INSTALL` is used to indicate whether the cert-manager should be installed or not. Set `SKIP_CERT_MANAGER_INSTALL=true` only when a compatible cert-manager is already present or the scenario does not require it:

```sh
make test-e2e-kind KIND_CLUSTER=hyperfleet-operator-dev SKIP_CERT_MANAGER_INSTALL=true
```

The test suite runs to completion or failure. On completion the Kind cluster gets deleted. On failure, the cleanup step is skipped and cluster can be debugged. Preserve any useful diagnostics, then remove the dedicated cluster explicitly:

```sh
make destroy-kind-cluster KIND_CLUSTER=hyperfleet-operator-dev
```

### E2E - CI

```sh
make test-e2e-ci
```

We run this target as a step in the presubmit prow job. A cluster will be "claimed" and KUBECONFIG points to it. The job has already built the operator and bundle and run `operator-sdk run bundle ...` before we run this target. We assume that the operator manifests and CRDs are installed and running, so the aforementioned steps in [the Kind flow above](#e2e---kind) are skipped.

`OPERATOR_NAMESPACE` is the namespace the operator and its
resources were installed into. It defaults to `hyperfleet-system` and must be
overridden if the target cluster installed them elsewhere, e.g.

```sh
make test-e2e-ci OPERATOR_NAMESPACE=...
```

If the test has completed successfully, the presubmit job will pass. If not the logs from the prow run will be logged in the PR checks section.

### E2E - Upgrades

For published N−1 → N bundles on a prepared isolated cluster, use the separate
[OLM Classic upgrade suite](upgrade-testing.md). Its ordinary helper tests run
under `make test`; live execution requires `make test-e2e-upgrade` and explicit
release/scenario inputs.

## Generated and release-facing files

Changes to API markers, RBAC markers, watched operand types, manager environment
variables, or related images have generated and overlay consequences. At
minimum:

1. Run `make manifests generate`.
2. Inspect changes under `config/crd/bases`, `config/rbac`, and generated Go
   files; do not edit those outputs by hand.
3. Build both development and production kustomize overlays when image or
   environment-variable ordering changes.
4. Validate the OLM bundle's `spec.relatedImages` for production delivery. See
   the [component image workflow](component-pattern.md#8-add-the-image-and-related-image-plumbing)
   and [OLM guide](olm.md).

## Troubleshooting local development

| Symptom | What to check |
|---|---|
| `make bundle-build` cannot find `envsubst` | Install gettext. The development bundle overlay is generated from `config/manifests/dev/patch-images.yaml` with `envsubst`; the generated `kustomization.yaml` is ignored by Git. |
| Controller tests cannot start envtest | Run `make test` with network access so `setup-envtest` can obtain the Kubernetes control-plane binaries matching the Go module's Kubernetes dependencies. |
| An e2e run leaves a Kind cluster behind | Preserve the failure diagnostics, then run `make destroy-kind-cluster KIND_CLUSTER=<dedicated-cluster-name>`. Do not use a shared or valuable cluster name: the normal successful path deletes it too. |
| Generated files change unexpectedly | Run `make manifests generate`, inspect the diff, and keep only changes caused by the API, RBAC, or controller markers you intentionally changed. |
| `make run` cannot reconcile the sample | Confirm that `OPERATOR_NAMESPACE` exists and contains the referenced Secrets, then check that the database is reachable from the manager and API workloads. |
