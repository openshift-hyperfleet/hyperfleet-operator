# Status conditions

`HyperFleetConfig` reports its installation health via `status.conditions`. This
is deliberately a *different* vocabulary from the HyperFleet API's own
condition dialect — the two are separate layers, and both are documented here
so it's clear which one you're looking at.

## Two layers

- **Operator layer** (this document): `status.conditions` on the
  `HyperFleetConfig` CR itself. It describes whether the operator has
  successfully installed and is maintaining a healthy deployment of the
  operand(s) selected by `spec.bundle` — i.e. "is the operator doing its job."
  This follows the OpenShift `ClusterOperator` convention (`Available`,
  `Progressing`, `Degraded`), minus `Upgradeable` (see architecture ADR-0019).
- **API layer**: conditions reported by the HyperFleet API's own resources
  (`Available`, `Ready`, `Reconciled`, `LastKnownReconciled`, per-adapter
  `Successful`, etc. — see architecture ADR-0007 and ADR-0008). These describe
  whether the API is successfully reconciling partner-managed resources
  (clusters, node pools, etc.) — i.e. "is the API doing its job." They are
  unrelated to and unaffected by the operator-layer conditions here.

A `HyperFleetConfig` can be `Available=True` (the API Deployment is healthy)
while individual API-layer resources are failing to reconcile, and vice versa:
the operator conditions say nothing about the health of resources managed
through the API.

## Operator-layer condition types

| Type | Meaning |
|---|---|
| `Available` | The operand (the API) is deployed and healthy. |
| `Progressing` | The operator is actively rolling out a change to the operand. |
| `Degraded` | The operator cannot reach or maintain the desired state. |

Each condition's `observedGeneration` (and the top-level
`status.observedGeneration`) reflects the `.metadata.generation` the operator
had processed when the condition was last evaluated. `lastTransitionTime`
changes only when a condition's `status` actually flips — reconciles that
leave health unchanged do not touch it.

If a reconcile fails before any component's health could actually be checked
(e.g. a failed OIDC discovery, or a failed apply on the first operand),
`Available` and `Progressing` are left exactly as they were last recorded
rather than being guessed — only `Degraded` and `observedGeneration` are
updated. This avoids publishing a fabricated "healthy" value alongside
`Degraded=True` when the real state was never actually observed.

## Reason strings

Every condition write uses one of the following reasons — no ad hoc strings.
Partners may read `status.conditions[].reason` as part of the published
contract, so this table is the source of truth; the Go constants live
alongside the condition types in `api/v1alpha1/hyperfleetconfig_types.go`.

| Condition | Status | Reason | Meaning |
|---|---|---|---|
| Available | True | `DeploymentAvailable` | The operand Deployment reports Available; all desired replicas are ready. |
| Available | False | `DeploymentUnavailable` | The operand Deployment is missing, or has zero available replicas. |
| Available | False | `DeploymentNotReady` | The operand Deployment exists with some, but not all, replicas ready. |
| Progressing | True | `RolloutInProgress` | The operand Deployment has not finished rolling out its current generation. |
| Progressing | False | `RolloutComplete` | The operand Deployment is fully rolled out and stable. |
| Degraded | False | `AsExpected` | No failure detected (the ClusterOperator convention's default). |
| Degraded | True | `ReferencedSecretMissing` | A Secret referenced by `spec.api` (database, TLS, or JWKS) does not exist in the operator's namespace. |
| Degraded | True | `ReconcileError` | Any other error during the most recent reconcile — JWKS discovery, reading referenced Secrets, resolving bundle components, or a component's render/apply. |

## Notes on `Available`

The operand's readiness probe (`/readyz`) only succeeds once the API has
established a working database connection, so `Available=True` also implies
the API can reach its configured PostgreSQL database — not just that the pod
is running (`/healthz`, the liveness probe, does not check this).
