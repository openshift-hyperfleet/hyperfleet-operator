# `HyperFleetConfig` reference

`HyperFleetConfig` is the complete partner-facing configuration and
installation-health API for the HyperFleet Operator. This document describes
the current `hyperfleet.redhat.com/v1alpha1` schema and its implemented behavior.
The generated CRD remains the admission-time source of truth.

## Contents

- [Resource identity](#resource-identity)
- [Example](#example)
- [Specification](#specification)
  - [TLS port advertisement](#tls-port-advertisement)
  - [Referenced Secret contracts](#referenced-secret-contracts)
  - [Authentication and JWKS selection](#authentication-and-jwks-selection)
  - [Current sizing limitation](#current-sizing-limitation)
- [Status reference](#status-reference)
  - [Per-component status](#per-component-status)
  - [Operator conditions versus API resource conditions](#operator-conditions-versus-api-resource-conditions)
  - [Operator condition types](#operator-condition-types)
  - [Condition reasons](#condition-reasons)

## Resource identity

| Property | Value |
|---|---|
| API version | `hyperfleet.redhat.com/v1alpha1` |
| Kind | `HyperFleetConfig` |
| Scope | Cluster |
| Short name | `hfc` |
| Required name | `cluster` |
| Status subresource | Enabled |

The object has no `metadata.namespace`. Its CEL validation fixes the name to
`cluster`, making it a cluster-scoped singleton.

## Example

```yaml
apiVersion: hyperfleet.redhat.com/v1alpha1
kind: HyperFleetConfig
metadata:
  name: cluster
spec:
  bundle: cloud-capi
  api:
    database:
      secretRef:
        name: hyperfleet-db
    auth:
      enabled: true
      issuer: https://issuer.example.com
      audience: hyperfleet-api
    tls:
      secretRef:
        name: hyperfleet-api-tls
    profile: small
```

All referenced Secrets belong in the operator's namespace, not in a namespace
on the CR. The default operator namespace is `hyperfleet-system`, but deployment
configuration can change it.

## Specification

| Field | Required | Default | Validation and behavior |
|---|---:|---|---|
| `spec.bundle` | Yes | None | Immutable. Accepted values are `cloud-capi` and `onprem-agent`. `onprem-agent` is a placeholder. The operator cannot resolve it yet, and reconciliation fails. Use `cloud-capi`. |
| `spec.api` | Yes | None | Partner-facing HyperFleet API configuration. |
| `spec.api.database` | Yes | None | External PostgreSQL configuration; the operator does not provision a database. |
| `spec.api.database.secretRef` | Yes | None | Name-only reference to the database Secret in the operator namespace. |
| `spec.api.database.secretRef.name` | Yes | None | DNS-1123 subdomain, 1 to 253 characters. |
| `spec.api.auth` | Yes | None | JWT authentication configuration. |
| `spec.api.auth.enabled` | No | `true` | Boolean. When true, both issuer and audience are required. |
| `spec.api.auth.issuer` | Conditional | None | Required when auth is enabled. Whenever present, it must be an HTTPS URL with a host and 1 to 2048 characters. |
| `spec.api.auth.audience` | Conditional | None | Required and non-empty when auth is enabled; 1 to 253 characters when present. |
| `spec.api.auth.jwkCertSecretRef` | No | None | Optional name-only reference to a Secret containing a JWKS document. |
| `spec.api.auth.jwkCertSecretRef.name` | Conditional | None | Required when the reference is present; DNS-1123 subdomain, 1 to 253 characters. |
| `spec.api.tls` | No | No `server.tls` block; API serves plain HTTP | Optional API-serving TLS configuration. See [TLS port advertisement](#tls-port-advertisement). |
| `spec.api.tls.secretRef` | Conditional | None | Required when `tls` is present; name-only reference in the operator namespace. |
| `spec.api.tls.secretRef.name` | Conditional | None | DNS-1123 subdomain, 1 to 253 characters. |
| `spec.api.profile` | No | `small` | Mutable. Accepted values are `small`, `medium`, and `large`. See [current sizing limitation](#current-sizing-limitation). |

### TLS port advertisement

When `spec.api.tls` is set, the API listener serves HTTPS and the rendered
operands advertise that: the API container port and the Service port are named
`https` (instead of `http`) and the Service sets `appProtocol: https`, so
Kubernetes-aware proxies and service meshes route to the listener as TLS.
Clients must call the API Service with `https://` in that case; a plaintext
request fails with `Client sent an HTTP request to an HTTPS server`. With
`spec.api.tls` unset, the ports keep their `http` names and no `appProtocol` is
set, leaving plain-HTTP routing unchanged.

Certificate hostname requirements: the Sentinels reach the API at its
in-cluster Service DNS name, so the certificate in the `tls.secretRef` Secret
must be valid for `hyperfleet-api.<operator-namespace>.svc`. A certificate
whose SANs do not include that name fails TLS verification in the Sentinel
clients.

Known limitation (see HYPERFLEET-1678): the API certificate is commonly issued
by a private CA (for example, cert-manager). The Sentinels currently have no
way to load and trust a custom CA, so with a private-CA-issued certificate the
Sentinel-to-API HTTPS connection fails certificate verification. Until
Sentinel support for trusting a custom CA lands and is wired into the
operator, `spec.api.tls` with a private CA is usable only for clients that
already trust that CA.

### Referenced Secret contracts

| Reference | Required Secret type | Required keys | Notes |
|---|---|---|---|
| `database.secretRef` | No type requirement | `db.host`, `db.port`, `db.name`, `db.user`, `db.password` | Always referenced by the rendered API Deployment. |
| `auth.jwkCertSecretRef` | No type requirement | `jwks.json` | Used only when authentication is enabled and this reference is set. |
| `tls.secretRef` | `kubernetes.io/tls` | `tls.crt`, `tls.key` | Used when `spec.api.tls` is present. |

The schema validates reference names, but it cannot validate another object's
namespace, existence, type, or keys. The reconciler resolves references in the
operator namespace. A missing referenced Secret produces
`Degraded=True/ReferencedSecretMissing`; the desired Deployment still contains
the reference and can recover when the Secret is created.

Creating, updating, or deleting a referenced Secret triggers a later reconcile.
The operator rolls the API pods when a referenced Secret changes so they use
current credential or certificate material, without copying Secret values into
rendered configuration.

### Authentication and JWKS selection

Authentication defaults to enabled. When it is enabled:

- Setting `jwkCertSecretRef` mounts `jwks.json` from that Secret.
- Omitting `jwkCertSecretRef` makes the operator fetch
  `{issuer}/.well-known/openid-configuration` and pass the returned `jwks_uri`
  to the API component.
- Discovery connections are limited to public destinations. The operator rejects
  loopback, private, link-local, multicast, unspecified, CGNAT, and special-
  purpose addresses that IANA does not mark **Globally Reachable: True**. The
  checked-in table is a complete static snapshot of the
  [IANA IPv4](https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml)
  and [IANA IPv6](https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml)
  registries, updated 2025-10-09. Records with `False`, `N/A`, or retired
  reachability are denied; IANA's explicitly globally reachable, more-specific
  exceptions remain allowed by longest-prefix match. For the globally reachable
  `64:ff9b::/96` NAT64 prefix, the embedded IPv4 destination must also pass the
  IPv4 policy, so translation cannot reach a blocked IPv4 address. This check is
  applied to the resolved dial address, so DNS rebinding cannot bypass it. Use
  `jwkCertSecretRef` for a private or air-gapped issuer.
- A discovery failure that has no usable cached value degrades reconciliation.

When `enabled` is explicitly false, issuer and audience are not required and
JWKS discovery is not needed. If issuer or audience are supplied anyway, their
individual validation still applies.

### Current sizing limitation

The schema accepts `small`, `medium`, and `large`, defaults to `small`, and
allows the field to change. The current API renderer does not branch on that
value: all three profiles render one replica with the same requests
(`100m` CPU, `128Mi` memory) and limits (`500m` CPU, `512Mi` memory), with no
profile-specific HPA or PDB. The field currently expresses future sizing
intent, not a sizing guarantee.

## Status reference

`status` contains:

| Field | Meaning |
|---|---|
| `status.observedGeneration` | Latest `.metadata.generation` for which the operator completed component-health collection and status rollup. |
| `status.conditions` | Map-like list keyed by condition `type`, using Kubernetes `metav1.Condition`. |
| `status.components` | Map-like list keyed by component `name`, carrying each bundle component's own conditions alongside the top-level rollup. |

### Per-component status

`status.components` preserves the originating component's identity so partners
can distinguish, for example, the API's health from Sentinel's without
inspecting individual Deployments. Each entry's `name` is the stable component
identifier: `api`, plus one `sentinel-<resourceType>` entry per registered
entity type (e.g. `sentinel-clusters`). Each entry's `conditions` use the
`Available` and `Progressing` types with the same reasons and
`metav1.Condition` shape as the operator rollup (`Degraded` remains a
rollup-level signal only, driven by controller-level reconcile errors and
missing referenced Secrets), and each condition's `observedGeneration` records
the `HyperFleetConfig` generation that was evaluated.

An entry is written only for a component whose health was actually observed in
a given reconcile. When a reconcile aborts early (for example, a later
component fails to apply), the successfully observed components' entries are
updated while the unobserved components' entries keep their previous values —
an entry whose `observedGeneration` lags `status.observedGeneration` was not
freshly evaluated by the most recent reconcile.

Pruning has two distinct triggers. Entries for components that no longer exist
in the resolved bundle are dropped on the next *fully successful* reconcile —
an aborted reconcile keeps them, since the unobserved remainder of the set is
unknown. Condition types a still-present component stops reporting are dropped
on the next reconcile that observes that component, except when it reports no
conditions at all: an empty report is treated as a contract violation rather
than an intentional wipe, so the component is skipped entirely — it keeps its
last-known conditions (a component that has never reported conditions never
gains an entry), and any retained conditions still feed the top-level rollup.
In that edge case the top-level
conditions' `observedGeneration` reflects the generation the rollup was
computed against, even though that component's input was last evaluated at an
earlier generation; the component's own entry keeps the truer stamp.

These are operator-layer conditions about the operand workloads. They are
distinct from the status conditions on HyperFleet API resources managed
*through* the API (see "Operator conditions versus API resource conditions"
below).

### Operator conditions versus API resource conditions

There are two independent condition layers:

- The **operator layer** is `HyperFleetConfig.status.conditions`. It answers
  whether the operator installed and is maintaining the selected operands. Its
  types are `Available`, `Progressing`, and `Degraded`, following the relevant
  OpenShift `ClusterOperator` convention from
  [ADR-0019](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0019-package-hyperfleet-as-operator.md).
- The **HyperFleet API layer** is status on resources managed through the API.
  Its resource-condition vocabulary is non-exhaustive: fixed types include
  `Available`, `Health`, `Reconciled`, `Finalized`, and
  `LastKnownReconciled`, plus per-adapter `<Adapter>Successful` conditions and
  mapped conditions when applicable. An API resource's `Available` condition is
  distinct from the operator's `HyperFleetConfig` `Available` condition. See
  [ADR-0007](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0007-conditions-based-status-model.md)
  and
  [ADR-0008](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/adrs/0008-dynamic-status-aggregation.md).

An operator `Available=True` does not assert that partner-managed resources are
reconciled. Conversely, an API resource's success does not establish that the
operator can maintain every operand.

### Operator condition types

| Type | True means | False means |
|---|---|---|
| `Available` | Every component reports available. For the current API component, all desired Deployment replicas are available. | At least one component is missing, unavailable, or not fully ready. |
| `Progressing` | At least one component is rolling out. | All observed components have completed rollout. |
| `Degraded` | The latest reconcile found a missing referenced Secret or another reconciliation error. | No failure signal was found. |

For the API component, `/readyz` requires a working database connection.
Consequently, `Available=True` implies the API Deployment has ready replicas
whose readiness checks can reach the configured PostgreSQL database; it is
stronger than the `/healthz` liveness check.

### Condition reasons

Every operator condition write uses one of these constants. Treat these strings
as published vocabulary rather than free-form text.

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| `Available` | `True` | `DeploymentAvailable` | All components are available; the API Deployment reports all desired replicas available. |
| `Available` | `False` | `DeploymentUnavailable` | An operand Deployment is absent or has zero available replicas. |
| `Available` | `False` | `DeploymentNotReady` | A Deployment has some, but not all, desired replicas available. |
| `Progressing` | `True` | `RolloutInProgress` | An operand Deployment has not completed rollout of its current generation. |
| `Progressing` | `False` | `RolloutComplete` | All observed operand Deployments are fully rolled out and stable. |
| `Degraded` | `False` | `AsExpected` | No reconciliation failure was detected. |
| `Degraded` | `True` | `ReferencedSecretMissing` | A database, TLS, or JWKS Secret reference does not resolve in the operator namespace. |
| `Degraded` | `True` | `ReconcileError` | Any other error prevented the latest reconcile from reaching or maintaining the desired state. |

Each condition's `observedGeneration` is the CR generation against which that
specific condition was evaluated. `lastTransitionTime` changes only when the
condition's status changes, not on every reconcile.

If reconciliation fails before component health can be checked, the operator
does not invent new `Available` or `Progressing` values and does not advance the
top-level `status.observedGeneration`. It re-evaluates only `Degraded` for the
new generation, preserving the last actually observed component health.
