# `ManagementClusterRegistration` reference

`ManagementClusterRegistration` is a Hub-owned binding between one remote
Applier's identity and one Desire Store partition. Only a Hub administrator
should create, modify or delete registrations. Never delegate registration
permissions to management-cluster identities: that would let them choose their
own partition and issuer trust.

The contract follows the
[Desire Store detailed design](https://github.com/openshift-hyperfleet/architecture/blob/main/hyperfleet/docs/desire-store-dd.md#partition-binding).

## Resource identity

| Property | Value |
|---|---|
| API version | `hyperfleet.redhat.com/v1alpha1` |
| Kind | `ManagementClusterRegistration` |
| Resource | `managementclusterregistrations` |
| Scope | Cluster; no `metadata.namespace` |
| Name | Administrator-selected, not a singleton |
| Caller class | Always `remote-applier`; not configurable |
| Status subresource | None |

## Specification

All four fields are required and mutable. Admission preserves their exact
values; in particular, it does not remove a trailing slash from an issuer URL.

| Field | Validation |
|---|---|
| `spec.issuerURL` | HTTPS URL with a host, 1–2048 characters; no credentials, whitespace, query or fragment. A path, port, trailing slash and private hostname are allowed. |
| `spec.subject` | Exact `system:serviceaccount:<namespace>:<name>` subject. Namespace is a DNS label of at most 63 characters; service-account name is a DNS subdomain of at most 253 characters. Wildcards and groups are not accepted. |
| `spec.audience` | 1–253 characters, without whitespace or ASCII control characters. A URI audience is permitted. |
| `spec.partition` | Lowercase DNS label, 1–63 characters, because the partition also selects the gateway hostname. No dots, wildcards or path separators. |

The issuer must be reachable by the gateway over the approved network path.
Schema validation checks syntax, not reachability, discovery or signing keys.

## Example

```yaml
apiVersion: hyperfleet.redhat.com/v1alpha1
kind: ManagementClusterRegistration
metadata:
  name: mc-01
spec:
  issuerURL: https://issuer.mc-01.example.com
  subject: system:serviceaccount:hyperfleet:applier
  audience: hyperfleet-desire-store
  partition: mc-01
```

See the [sample manifest](../config/samples/hyperfleet_v1alpha1_managementclusterregistration.yaml).
It is deliberately excluded from `config/samples/kustomization.yaml`: review
and replace its placeholder binding values before applying the file explicitly.

## Administrative access

The dedicated
[ClusterRole](../config/rbac/managementclusterregistration_admin_role.yaml)
permits `create`, `get`, `list`, `watch`, `update`, `patch` and `delete` only on
registration records. It grants no access to Secrets, AuthConfigs, role
bindings or other HyperFleet resources. The operator's manager service account
does not need registration write access for this API-only implementation.

The role has no automatic binding and is not aggregated into Kubernetes'
generic `admin`, `edit` or `view` roles. A cluster administrator must explicitly
delegate it to trusted Hub administrators with a ClusterRoleBinding. A
namespace RoleBinding does not grant access to this cluster-scoped resource.

For a manifest-based installation (`make deploy`), Kustomize installs the role
as `hyperfleet-operator-managementclusterregistration-admin-role`:

```bash
kubectl create clusterrolebinding hyperfleet-registration-admins \
  --clusterrole=hyperfleet-operator-managementclusterregistration-admin-role \
  --group=hyperfleet-hub-registration-admins
```

Kubernetes RBAC is additive: cluster administrators and any separately granted
wildcard permissions still have access. This role is not an admission deny
policy and cannot override another role's grants. Audit other role bindings
when onboarding management clusters; never bind this role to them.

### OLM bootstrap and lifecycle

OLM automatically creates roles aggregated into generic `admin`, `edit` and
`view` for APIs listed as owned in a CSV. To avoid that implicit delegation,
the registration CRD is intentionally absent from both the owned and required
APIs in the HyperFleet CSV. A required API creates a provider dependency in
OLM's resolver; a manually installed, unowned CRD does not satisfy that dependency.
The bundle excludes the CRD, administrator role and example
registration. An administrator installs and maintains the registration API
separately, before creating the Subscription or running `operator-sdk run bundle`:

```bash
make install-registration-api
```

This applies the CRD and role files on disk without regenerating them or running
`controller-gen`. Use an unmodified checkout of the matching release; the target
does not authenticate local files. Developers regenerate artifacts separately
with `make manifests`.

The target installs the registration CRD, waits for it to be established,
and installs the unbound role as `managementclusterregistration-admin-role`
(without the manifest deployment's `hyperfleet-operator-` prefix). Delegate
this bootstrap role explicitly if needed:

```bash
kubectl create clusterrolebinding hyperfleet-registration-admins \
  --clusterrole=managementclusterregistration-admin-role \
  --group=hyperfleet-hub-registration-admins
```

Use the CRD and role manifests from the same release as the operator. For
disconnected installation, transfer these two files through the approved release
artifact channel and apply them locally; the catalog mirror does not carry them:

```bash
kubectl apply -f config/crd/bases/hyperfleet.redhat.com_managementclusterregistrations.yaml
kubectl wait --for=condition=Established \
  crd/managementclusterregistrations.hyperfleet.redhat.com --timeout=60s
kubectl apply -f config/rbac/managementclusterregistration_admin_role.yaml
```

OLM does not enforce this external bootstrap: a successful operator installation
does not prove that the registration API or role exists. Verify bootstrap
separately before administering registration records. OLM uninstall does not
remove this external CRD or its records. Upgrade the external CRD/RBAC
explicitly alongside releases; do not delete the CRD during a routine uninstall,
because deletion destroys all registrations. Do not publish a separate OLM
provider that owns this CRD: that would reintroduce automatic RBAC aggregation.

### Manifest-based uninstall

`make install` and `make deploy` include the registration CRD. Routine
`make uninstall` and `make undeploy` use dedicated deletion overlays that exclude
it, retaining all registrations. `make undeploy` still removes the unbound
administrator role installed by `make deploy`; reapply it before administering
the retained records. Other operator resources, including the HyperFleetConfig
CRD, retain their existing removal behavior.

Run removal targets from the same release checkout used for installation. Both
broker variants have the same resource identities, so they share the deletion
overlay. Do not use `kubectl delete -f dist/install.yaml` or delete
`config/default`/`config/crd` directly: those installation inputs still include
the registration CRD and deleting it permanently destroys its records.

The packaging tests check manifest output and deletion inputs, not live
record retention or an OLM installation. Live
OLM acceptance must also confirm that no other operator owns the registration
API and that generic cluster-bound `admin`/`edit` users cannot create records.

## Implementation boundary

This change provides the API, declarative admission validation and administrative
RBAC for [HYPERFLEET-1830](https://redhat.atlassian.net/browse/HYPERFLEET-1830).
Creating a record does not yet enable Desire Store access, and deleting one
does not yet revoke an AuthConfig: rendering, replacement and removal of those
policies belong to
[HYPERFLEET-1831](https://redhat.atlassian.net/browse/HYPERFLEET-1831).
Cross-record issuer uniqueness and overlapping signing-key rejection belong to
[HYPERFLEET-1832](https://redhat.atlassian.net/browse/HYPERFLEET-1832), not this
CRD's single-object validation. No binding-readiness status is invented here.
