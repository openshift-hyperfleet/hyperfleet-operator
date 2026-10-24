# Adding a component

This guide defines the repository contract for adding an operand component to
the HyperFleet Operator. A component is a package that renders Kubernetes
objects and reports health; it is not a new controller.

In this document, **operator bundle definition** means the ordered component set
selected by `spec.bundle` in `internal/bundle`. It is different from the **OLM
bundle**, the release artifact under `bundle`/`config/manifests`.

## Contents

- [Component contract](#component-contract)
- [Implementation sequence](#implementation-sequence)
  - [Classify the component](#1-classify-the-component)
  - [Create the package and constructor](#2-create-the-package-and-constructor)
  - [Extend resolver inputs and the bundle definition](#3-extend-resolver-inputs-and-the-bundle-definition)
  - [Register object kinds, watches, cache access, and RBAC](#4-register-object-kinds-watches-cache-access-and-rbac)
  - [Integrate configuration and rollout behavior](#5-integrate-configuration-and-rollout-behavior)
  - [Define health and status rollup](#6-define-health-and-status-rollup)
  - [Add observability](#7-add-observability)
  - [Add the image and related-image plumbing](#8-add-the-image-and-related-image-plumbing)
- [Required verification](#required-verification)
- [Structural review checklist](#structural-review-checklist)

## Component contract

Every component implements `internal/bundle.Component`:

```go
type Component interface {
    Name() string
    Render(context.Context, *v1alpha1.HyperFleetConfig) ([]client.Object, error)
    Conditions(context.Context, *v1alpha1.HyperFleetConfig, []client.Object) ([]metav1.Condition, error)
}
```

`Render` is a pure desired-state function. It must not read from or write to the
cluster. Return typed objects with complete `TypeMeta`, stable names and
namespaces, labels/selectors, and deterministic ordering. The controller adds
the `HyperFleetConfig` controller owner reference and applies objects with
server-side apply and force ownership.

`Conditions` is also cluster-I/O-free. `internal/apply.Objects` refreshes the
rendered objects in place after applying them, so this method must derive health
from the supplied `applied` slice. It should return the component-level
`Available` and `Progressing` observations needed by the controller's rollup and
must use the published reason constants. Controller errors and missing Secret
references are rolled into `Degraded` centrally.

## Implementation sequence

### 1. Classify the component

Decide whether the operand is:

- **Shared tier**: present for every usable bundle and registered by
  `sharedTier`.
- **Bundle-specific**: selected only for one or more bundle values and
  registered by `bundleSpecific`.

Do not expose internal machinery in `HyperFleetConfig` merely because a new
component needs configuration. The CR contains durable partner intent, not
implementation layout. Any new field requires an API compatibility review,
validation/defaulting tests, regenerated CRDs, sample and reference updates,
and an explanation of why an operator-internal default is insufficient.

### 2. Create the package and constructor

Add a focused package under `internal/component/<name>`. Follow the API
component's separation between component construction/health and manifest
rendering when useful. The constructor should accept only resolved inputs it
needs, such as image, namespace, or operator-derived configuration.

Add unit tests for at least:

- stable component name and default image behavior;
- the complete rendered object set and TypeMeta;
- labels, selectors, service accounts, volumes, ports, probes, resources, and
  security context relevant to the workload;
- optional and invalid configuration paths;
- missing, unavailable, partially ready, rolling, and stable status states;
- deterministic rendering.

### 3. Extend resolver inputs and the bundle definition

Add constructor inputs to `internal/bundle.Config`, wire them from
`HyperFleetConfigReconciler`, and populate them in `cmd/main.go` where
appropriate. Register the component in `sharedTier` or `bundleSpecific` in its
dependency-safe order. Update resolver tests for every bundle value, component
order, constructor error, and incomplete component configuration. A bundle
definition must never silently resolve to a nonfunctional component set.

### 4. Register object kinds, watches, cache access, and RBAC

If the component introduces a Kubernetes kind not already returned by
`controller.NamespacedOperandTypes`, add a fresh typed object there. That list
drives both controller `.Owns(...)` watches and cache scoping; adding RBAC alone
is insufficient.

Add or adjust kubebuilder RBAC markers in the controller and run
`make manifests`. Confirm the generated cluster role, namespaced operand role,
and role bindings grant only required verbs. Update tests that assert watched
types, cache configuration, or RBAC parity. Cluster-scoped operand kinds need an
explicit architecture and ownership review rather than being inserted into the
namespaced list.

### 5. Integrate configuration and rollout behavior

Identify every ConfigMap and Secret input that must restart pods when it
changes. Missing Secrets must remain distinguishable from present-but-empty
inputs, and sensitive values must not be copied into hashes, logs, metrics, or
rendered nonsensitive configuration.

The current `stampConfigHash` implementation recognizes only the API
component's well-known ConfigMap and Deployment names. A second configurable
workload does **not** receive rollout annotations automatically. Generalize the
rollout-hash contract and add cross-component tests instead of copying API names
or assuming the current helper covers the new component.

### 6. Define health and status rollup

Use current Deployment generation, desired/updated/available replica counts,
and old replicas to distinguish unavailable, partially ready, progressing, and
complete rollout states. If a component is not Deployment-backed, define an
equivalent, testable readiness contract before implementation.

Prefer the existing operator-level condition types and reasons. If a genuinely
new published reason is necessary, add a constant, update every writer and
rollup test, and update the
[`HyperFleetConfig` reference](hyperfleetconfig-reference.md#condition-reasons)
in the same change.

### 7. Add observability

Ensure operator-level reconcile duration and error metrics retain the standard
`component="operator"` label. Operand readiness and rollout metrics carry the
stable component name in their `operand` label, using the same identity exposed
as `app.kubernetes.io/component` on rendered operand objects. Ensure the
applied-configuration hash includes relevant rendered and referenced inputs.
Update [the metrics reference](metrics.md) for any new metric or label value
contract. Never place credentials or Secret data in labels.

### 8. Add the image and related-image plumbing

If the component has a container image, define one stable
`RELATED_IMAGE_HYPERFLEET_<COMPONENT>` environment variable and trace it all the
way through:

1. Component default and environment-variable constant. A component may instead
   have no compiled-in default and fail on an empty image: Sentinel's `Render`
   returns an error when its image is empty.
2. `cmd/main.go`, reconciler fields, and `bundle.Config`.
3. Makefile default/export and image/build targets.
4. `config/manager/kustomization.yaml`.
5. `config/manifests/dev/patch-images.yaml`.
6. `config/manifests/prod/kustomization.yaml` and the generated CSV's
   `spec.relatedImages`. operator-sdk generates `spec.relatedImages` from the
   manager's `RELATED_IMAGE_*` environment variables, so the CSV base needs no
   edit.
7. Unit/e2e expectations and disconnected-delivery documentation where the
   release workflow changes.

The dev and prod overlays currently use positional JSON-patch paths for manager
environment variables. Inserting or reordering entries can patch the wrong
array item while still producing valid YAML. Render and inspect both overlays;
do not validate only the base.

Each overlay `replace` writes a whole `{name, value}` object, so it discards
whatever entry held that index, whatever its name. The overlays must therefore
cover exactly the base's `RELATED_IMAGE_*` indices, though not necessarily in
the base's order. Check the rendered environment by variable name, not by index.

For production, related images must be digest-pinned and must match every
operand image and every `RELATED_IMAGE_` value in the CSV. The validator under
`validators/related-images` enforces that relationship. See
[Adding a component image to the nudge chain](olm.md#adding-a-component-image-to-the-nudge-chain)
for the cross-repository nudge workflow and merge order.

## Required verification

Run these from the repository root:

```sh
make manifests generate
make lint
make test
make test-e2e KIND_CLUSTER=hyperfleet-operator-component-dev
```

Use a dedicated e2e cluster name; the successful target deletes it, while a
failed Go test leaves it behind for diagnosis. See the
[developer guide](developer-guide.md#end-to-end-tests).

Render the manager and production OLM overlay with the repository-pinned
kustomize tool and inspect all images, environment variables, RBAC objects, and
CSV related images:

```sh
go tool -modfile=tools/go.mod kustomize build config/manager
go tool -modfile=tools/go.mod kustomize build config/manifests/prod
```

The development OLM overlay is intentionally generated and git-ignored; a fresh
checkout contains only `config/manifests/dev/patch-images.yaml`. Materialize the
same `kustomization.yaml` that `make bundle-build` uses before rendering it:

```sh
export RELATED_IMAGE_HYPERFLEET_API=<api-image>
export RELATED_IMAGE_HYPERFLEET_OPERATOR=<operator-image>
export RELATED_IMAGE_HYPERFLEET_SENTINEL=<sentinel-image>

envsubst < config/manifests/dev/patch-images.yaml > config/manifests/dev/kustomization.yaml
go tool -modfile=tools/go.mod kustomize build config/manifests/dev
```

The production overlay may require the digest-valued substitutions documented
in the OLM guide. A successful YAML render alone does not prove its image
contract is valid.

## Structural review checklist

Before requesting review, verify the change as one complete vertical slice:

- The component has a single responsibility and obeys the pure render/health
  contract.
- Every supported bundle resolves to the intended ordered set, and incomplete
  bundles fail explicitly.
- New kinds are watched, cached, owned, and authorized consistently.
- Apply behavior, garbage collection, immutable-field handling, and rollout
  triggers are tested.
- Status rollup stays correct with mixed healthy, progressing, and failed
  components.
- Image values have one unbroken path from build input to rendered operand and
  OLM `relatedImages`.
- Development, production, connected, and disconnected render paths are not
  accidentally coupled to local defaults.
- CRD, RBAC, deep-copy, samples, documentation, and generated OLM output agree.
- Unit, envtest, lint, and dedicated-cluster e2e checks pass.
- A reviewer who did not implement the component can use the docs and tests to
  explain its registration, lifecycle, health, and release impact.
