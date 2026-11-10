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
| `spec.issuerURL` | HTTPS URL with a host, 1–2048 characters; no credentials, ASCII whitespace/control characters, query or fragment. A path, port, trailing slash and private hostname are allowed. |
| `spec.subject` | Exact `system:serviceaccount:<namespace>:<name>` subject. Namespace is a DNS label of at most 63 characters; service-account name is a DNS subdomain of at most 253 characters. Wildcards and groups are not accepted. |
| `spec.audience` | 1–253 characters, without ASCII whitespace or control characters (U+0000–U+0020 and U+007F). A URI audience is permitted. |
| `spec.partition` | Lowercase DNS label, 1–63 characters, because the partition also selects the gateway hostname. No dots, wildcards or path separators. |

The issuer must be reachable by the gateway over the approved network path.
Schema validation checks syntax, not reachability, discovery or signing keys.

The complete subject is limited to 339 characters: the `system:serviceaccount:`
prefix, a namespace of at most 63 characters, a colon and an account name of at
most 253 characters. Account-name validation follows Kubernetes
`ValidateServiceAccountName`: the 253-character limit applies to the complete
name, without a separate 63-character cap on dot-separated segments.

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

The role has no automatic binding or aggregation labels. A cluster administrator
can explicitly delegate it to trusted Hub administrators with a
ClusterRoleBinding for registration-only access. In an OLM installation, OLM
also creates its own roles aggregated into generic `admin`, `edit` and `view`:
cluster-wide `admin`/`edit` principals are trusted registration administrators,
and cluster-wide `view` principals can read records. On OpenShift, the
built-in `cluster-reader` ClusterRole aggregates `aggregate-to-view`, so
`cluster-reader` principals can also read records; include them when auditing
who can see registrations. A namespace RoleBinding does not grant access to
this cluster-scoped resource.

Both the manifest installation (`make deploy`) and OLM bundle include the
unbound role as `hyperfleet-operator-managementclusterregistration-admin-role`:

```bash
kubectl create clusterrolebinding hyperfleet-registration-admins \
  --clusterrole=hyperfleet-operator-managementclusterregistration-admin-role \
  --group=hyperfleet-hub-registration-admins
```

Kubernetes RBAC is additive: cluster administrators and any separately granted
wildcard permissions still have access. This role is not an admission deny
policy and cannot override another role's grants. Audit other role bindings
when onboarding management clusters; never bind this role to them.

### OLM installation and lifecycle

The CSV owns `ManagementClusterRegistration`, just like `HyperFleetConfig`.
The bundle carries the registration CRD and unbound administrator role; OLM
installs them through the normal InstallPlan. CRD changes are delivered in
release bundles and upgraded through the Subscription, without a separate
Make command or external manifest transfer. Disconnected installations mirror
the same bundle through the catalog.

OLM's generic-role aggregation is intentional. Do not grant cluster-wide
`admin`/`edit` or other registration-write permissions to remote management-cluster
identities. The dedicated role remains useful when registration-only access is
preferable to a generic administrative role. Manifest-only installations do not
run OLM and therefore do not acquire OLM-generated aggregation roles.

Deleting the Subscription and the CSV leaves CRDs and their records in place; do
not delete the registration CRD to uninstall or upgrade the operator. Deleting
the CRD destroys all registrations. `operator-sdk cleanup` is not equivalent: it
removes the resources the CSV owns, including the registration CRD, and with it
every record. Back up the records first, and stop if the backup fails; writing
to a temporary file keeps any existing backup intact:

```bash
kubectl get managementclusterregistrations -o yaml \
  > managementclusterregistrations-backup.tmp \
  && mv managementclusterregistrations-backup.tmp \
  managementclusterregistrations-backup.yaml
```

Verify retention against the deployed OLM version. A bundle includes the API
and role, not a registration instance: apply a reviewed record explicitly after
installation.

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

The packaging tests check the bundled CRD, owned CSV declaration, unbound role
and deletion inputs, not live record retention or an OLM installation.
Both removal overlays use the shared `config/components/preserve-registration`
Kustomize component. Schema pattern tests exercise RE2 and, when Node.js is
available, ECMA-262 against the generated CRD; the JavaScript subtest is skipped
without Node.js. The Kind E2E suite tests manifest-only installation and absence
of OLM-generated access, not the OLM path. Live OLM acceptance must confirm that
the CRD and role install from the bundle, cluster-bound `admin`/`edit` users can
administer records, `view` users can read but not write, remote identities
without those grants cannot self-register, and records survive routine uninstall.

## Implementation boundary

This change provides the API, declarative admission validation and administrative
RBAC for [HYPERFLEET-1830](https://redhat.atlassian.net/browse/HYPERFLEET-1830).
Creating a record does not yet enable Desire Store access, and deleting one
does not yet revoke an AuthConfig: rendering, replacement and removal of those
policies belong to
[HYPERFLEET-1831](https://redhat.atlassian.net/browse/HYPERFLEET-1831).
Cross-record issuer/partition uniqueness and overlapping signing-key rejection belong to
[HYPERFLEET-1832](https://redhat.atlassian.net/browse/HYPERFLEET-1832), not this
CRD's single-object validation. No binding-readiness status is invented here.
