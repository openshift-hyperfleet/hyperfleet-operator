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

- Go 1.26.0, matching `go.mod` (the pinned golangci-lint does not support Go 1.27; on newer toolchains, prefix commands with `GOTOOLCHAIN=go1.26.0`).
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
  - [Running the manager from the host](#running-the-manager-from-the-host)
  - [Running the manager in a cluster](#running-the-manager-in-a-cluster)
- [End-to-end tests](#end-to-end-tests)
- [Generated and release-facing files](#generated-and-release-facing-files)
- [Troubleshooting local development](#troubleshooting-local-development)

## Repository layout

| Path | Responsibility |
|---|---|
| `api/v1alpha1` | `HyperFleetConfig` Go API, validation markers, defaults, status vocabulary, and generated deep-copy code. |
| `cmd` | Manager startup, scheme registration, flags, related-image input, and controller wiring. |
| `internal/controller` | Reconciliation, watches, Secret resolution, OIDC discovery, server-side apply orchestration, rollout hashing, status rollup, and metrics. |
| `internal/bundle` | Resolves `spec.bundle` into an ordered set of components. |
| `internal/component` | Component implementations. Each renders desired objects and derives health from the applied objects. |
| `internal/apply` | Owner-reference handling and server-side apply. |
| `config/crd` | Generated CRD manifests. Do not hand-edit generated bases. |
| `config/rbac` | Generated RBAC plus role aggregation and namespace-scoped operand access. |
| `config/manager` | Base manager Deployment and image substitutions. |
| `config/manifests` | OLM CSV bases and development/production overlays. |
| `config/samples` | Example `HyperFleetConfig`. |
| `test/e2e` | Kind-based end-to-end setup and Ginkgo suite. |
| `validators` | Bundle validation, including digest-pinned related images. |
| `hack` | Build, validation, and disconnected-mirroring helpers. |
| `tools` | Pinned Go tool dependencies used by the Makefile. |

## Reconciliation model

There is one controller for the singleton `HyperFleetConfig`; components do not
get independent controllers. At a high level, one reconciliation:

1. Reads `HyperFleetConfig/cluster` and establishes the operator namespace.
2. Resolves referenced Secrets and, when needed, the issuer's OIDC discovery
   document.
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

### Running the manager from the host

With a usable current kubeconfig:

```sh
make install
make run OPERATOR_NAMESPACE=hyperfleet-system
```

The namespace must exist and must contain every Secret referenced by the
cluster-scoped CR. `make run` executes the manager on the host; it does not
create the namespace, database, credentials, issuer, or certificates. Apply a
sample only after replacing its example values with reachable dependencies.

### Running the manager in a cluster

To test the manager as an in-cluster Deployment without building an OLM bundle,
first build and push an operator image to a registry the cluster can pull. Then
use the direct-deployment target with explicit operator and API images:

```sh
make deploy \
  RELATED_IMAGE_HYPERFLEET_OPERATOR=<operator-image> \
  RELATED_IMAGE_HYPERFLEET_API=<api-image> \
  RELATED_IMAGE_HYPERFLEET_SENTINEL=<sentinel-image> \

kubectl rollout status deployment/hyperfleet-operator-controller-manager \
  -n hyperfleet-system
```

`make deploy` renders `config/default` into `dist/install.yaml`, including the
CRD, RBAC, manager Deployment, and metrics Service, then applies it. The
deployment uses `hyperfleet-system` by default. This path does not provision
dependency Secrets, a database, issuer, or certificates; create a configuration
only after providing reachable dependencies.

After creating those dependencies and replacing the sample's placeholder
values, apply the example configuration:

```sh
kubectl apply -k config/samples/
```

To remove a local installation, delete the example resource first, then remove
the CRD and manager:

```sh
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## End-to-end tests

Use a dedicated Kind cluster name that contains no valuable workloads:

```sh
make test-e2e KIND_CLUSTER=hyperfleet-operator-dev
```

The target creates the named cluster only when it does not already exist,
builds and loads the manager image through Docker, installs the generated CRD
and manager, installs cert-manager by default, and runs the Ginkgo suite. On a
successful run it deletes the selected Kind cluster. This means an existing
same-named cluster is reused and then deleted; never point the target at a
shared cluster.

If the Go test command fails, Make stops before the cleanup recipe. Preserve any
useful diagnostics, then remove the dedicated cluster explicitly:

```sh
make cleanup-test-e2e KIND_CLUSTER=hyperfleet-operator-dev
```

The e2e suite gathers pod logs, events, and resource descriptions on failure.
Set `CERT_MANAGER_INSTALL_SKIP=true` only when a compatible cert-manager is
already present or the scenario does not require it:

```sh
make test-e2e KIND_CLUSTER=hyperfleet-operator-dev CERT_MANAGER_INSTALL_SKIP=true
```

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
| An e2e run leaves a Kind cluster behind | Preserve the failure diagnostics, then run `make cleanup-test-e2e KIND_CLUSTER=<dedicated-cluster-name>`. Do not use a shared or valuable cluster name: the normal successful path deletes it too. |
| Generated files change unexpectedly | Run `make manifests generate`, inspect the diff, and keep only changes caused by the API, RBAC, or controller markers you intentionally changed. |
| `make run` cannot reconcile the sample | Confirm that `OPERATOR_NAMESPACE` exists and contains the referenced Secrets, then check that the database and, when authentication is enabled, the issuer are reachable from the manager and API workloads. |
