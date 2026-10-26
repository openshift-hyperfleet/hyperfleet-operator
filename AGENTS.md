# HyperFleet Operator — Agent Instructions

This Go module packages and operates HyperFleet through OLM using
controller-runtime. Its public API is the cluster-scoped singleton
`HyperFleetConfig/cluster`; operand workloads are operator-owned implementation
details. Use this file as the canonical repository guidance. `CLAUDE.md` only
imports it for Claude Code compatibility.

## Before changing code

- Read [the developer guide](docs/developer-guide.md) for repository layout,
  prerequisites, and the local workflow.
- Read [the CR reference](docs/hyperfleetconfig-reference.md) before changing
  `api/v1alpha1`, validation, defaults, Secret contracts, or status vocabulary.
- Read [the component pattern](docs/component-pattern.md) before adding an
  operand; verify its descriptions against the current `internal/bundle` and
  controller code rather than copying an older example blindly.
- Use [the OLM guide](docs/olm.md) and
  [disconnected-install guide](docs/disconnected-install.md) for packaging and
  release changes. An operator bundle definition in `internal/bundle` is an
  ordered component set, not an OLM bundle artifact.
- Follow the shared [architecture standards](https://github.com/openshift-hyperfleet/architecture/tree/main/hyperfleet/standards),
  especially [generated code](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/standards/generated-code-policy.md),
  [logging](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/standards/logging-specification.md),
  and [commits](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/standards/commit-standard.md).

## Architecture boundaries

- Keep one bundle controller in `internal/controller`. Add operands through
  `internal/component/<name>` and `internal/bundle`, not independent controllers.
- Component `Render` and `Conditions` methods must remain cluster-I/O-free.
  Render typed objects with complete TypeMeta and deterministic output;
  derive health from the supplied applied objects. Cluster reads, discovery,
  rollout orchestration, and status rollup belong in the controller.
- Use `internal/apply` for server-side apply and controller owner references.
  Preserve drift correction and owner-reference garbage collection; do not add
  finalizers or imperative cleanup without a lifecycle requirement.
- Operands and referenced Secrets live in the operator namespace. When adding
  an operand kind, keep `NamespacedOperandTypes`, watches, cache scoping, and
  generated RBAC aligned. Referenced partner Secrets are read-only, not owned.
- Each component declares `Rollout()` and `ReferencedSecrets(...)` through
  `bundle.Component`. Keep rollout hashes scoped to that component's inputs;
  never expose Secret contents in logs, metrics, or nonsensitive configuration.
- Keep durable partner intent in the CR, not internal deployment layout. Use
  existing condition/reason constants from `api/v1alpha1`, and update API tests,
  samples, generated manifests, and the reference when changing that contract.
- When changing operand images, keep `RELATED_IMAGE_*` wiring, manager and OLM
  overlays, and production `spec.relatedImages` consistent. Follow the component
  guide's image checklist and validate with `validators/related-images`.

## Commands and verification

Use Go 1.26.0 as specified in `go.mod`. With a newer local Go, prefix commands
with `GOTOOLCHAIN=go1.26.0`; do not upgrade pinned tools to work around local
toolchain incompatibility. The Makefile invokes tools pinned in `tools/go.mod`.

| Command | Purpose |
| --- | --- |
| `make help` | List supported targets; there is no aggregate `make check`. |
| `make install-hooks` | Install pre-commit and commit-msg hooks. |
| `make manifests generate` | Regenerate CRDs/RBAC and Go deep-copy implementations after marker or API changes. |
| `make lint-config lint` | Verify the linter schema and run the pinned linter. |
| `make test` | Regenerate, format, vet, obtain matching envtest assets, and run all non-E2E Go packages with coverage. |
| `make build` | Compile the manager into `bin/manager`. |
| `make run OPERATOR_NAMESPACE=hyperfleet-system` | Run locally against the current kubeconfig; requires dependencies and Secrets. |
| `pre-commit run --all-files` | Run pre-commit-stage checks; commit-msg validation is separate. |

Run lint, `make test`, and build before requesting review. Inspect the diff
after generation, tests, and hooks: these commands may rewrite files. Never
hand-edit generated CRD bases, RBAC output, or `zz_generated.deepcopy.go`.
Change the source markers and regenerate instead.

- Pure rendering, configuration, bundle, and helper behavior belongs in
  colocated unit tests. Controller/CRD integration tests use Ginkgo/Gomega and
  envtest; `make test` includes them. Envtest runs a control plane, not workload
  controllers, so it does not prove pod readiness or garbage collection.
- `test/e2e` contains Kind-based Ginkgo tests plus ordinary Go helper tests.
  `make test` excludes this package. Run its helper tests explicitly when
  changing them; compile or dry-run alone is not live E2E validation.
- For controller, operand, or deployment behavior changes, run
  `make test-e2e KIND_CLUSTER=<dedicated-disposable-name>` with the required
  container runtime and cluster tools. The target reuses an existing named
  cluster and deletes it on success. Never choose a shared cluster. On failure,
  preserve diagnostics before `make cleanup-test-e2e KIND_CLUSTER=<same-name>`.
- For documentation-only changes, validate links, commands, and source claims;
  report which code checks ran and whether envtest or live E2E was not run.

## Change discipline

Keep changes scoped, preserve unrelated work, and do not publish credentials or
Secret data in fixtures or diagnostics. Do not modify OLM artifacts, tool pins,
or dependency versions incidentally while editing guidance. Use `OWNERS` for
review routing and distinguish local verification, CI results, and live-cluster
acceptance in PR evidence.
