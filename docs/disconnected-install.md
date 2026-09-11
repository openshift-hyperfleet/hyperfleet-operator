# Disconnected installation from a published catalog

This workflow starts with a **published HyperFleet operator catalog**. The
catalog selects the bundles; their published image metadata determines the
operator and operand images to mirror. The CSV in this repository is a
development/build template, not the published bundle manifest.

Catalog publication is a prerequisite owned by the catalog publishing workflow. Obtain
the catalog digest, package, channel, and supported OpenShift/oc-mirror versions
from the catalog publisher. The example below uses package
`hyperfleet-operator` and channel `stable`; confirm these against the published
catalog. A bundle image alone cannot substitute for the catalog.

## 1. Prepare on the connected host

Requirements:

- The publisher-supported oc-mirror v2 binary and source registry credentials.
- A published catalog containing the intended release bundle and its related
  images, all accessible to the mirroring account.
- A destination registry reachable by the disconnected cluster and import host.
- An existing OpenShift cluster with OLM and its platform images provisioned for
  disconnected operation. Platform mirroring is a separate prerequisite.
- Registry authentication and CA trust configured for the import host and
  cluster. Configuring the host alone does not configure cluster image pulls.

Use the same oc-mirror version for export and import. OpenShift 4.17 documents
oc-mirror v2 as Technology Preview; the local
test helper uses 4.18.18. Confirm the supported tool/cluster combination for
your release before customer installation. Use a fresh
workspace to avoid an incremental archive that depends on an earlier transfer:

```bash
export MIRROR_ROOT="$(mktemp -d)"
cp docs/examples/imageset-config-catalog.yaml "$MIRROR_ROOT/imageset-config.yaml"
export REGISTRY_AUTH_FILE='/path/to/source-auth.json'
```

`oc-mirror` requires a containers-style auth file containing inline `auth`
entries. Create the file on the connected host with `skopeo login --authfile
"$REGISTRY_AUTH_FILE" quay.io`, or export an equivalent pull-secret; do not
transfer or commit it.

Replace the catalog placeholder with the published digest and confirm the
package/channel. The example selects the channel head in that immutable catalog
snapshot. Select a publisher-supported version range if you need older bundles
or an upgrade path.

Use `mirror.operators`. Do not enumerate the bundle, operator and API under
`additionalImages`: that bypasses catalog bundle and related-image discovery.
The catalog digest is the only release input to `oc-mirror`; it selects the OLM
bundle and its related images. The repository CSV is not an installation input.
List any dependent operator packages explicitly; `oc-mirror` does not infer
inter-operator dependencies. Do not use `skipDependencies` or blocked-image
filters to suppress required release content.

## 2. Export and transfer

```bash
oc-mirror --v2 --authfile "$REGISTRY_AUTH_FILE" \
  --config "$MIRROR_ROOT/imageset-config.yaml" \
  file://"$MIRROR_ROOT/archive"
tar -C "$MIRROR_ROOT" -czf /media/transfer/hyperfleet-mirror.tgz \
  archive imageset-config.yaml
sha256sum /media/transfer/hyperfleet-mirror.tgz
```

Record the printed archive digest in the publisher's authenticated release
system. A checksum copied only alongside the archive does not authenticate it.
If the publisher provides a signed checksum or manifest instead, verify that
signature with the publisher's release key and use its archive digest. Transfer
the complete archive and configuration; source credentials are not part of the
transfer artifact. Keep the archive digest, catalog digest, configuration, tool
version and export logs as release evidence.

## 3. Import on the disconnected host

Configure destination authentication and CA trust on this host. Use a fresh
directory and a destination reachable from every cluster node:

```bash
export IMPORT_ROOT="$(mktemp -d)"
export EXPECTED_ARCHIVE_SHA256='<digest from the authenticated publisher record>'
printf '%s  %s\n' "$EXPECTED_ARCHIVE_SHA256" \
  /media/transfer/hyperfleet-mirror.tgz | sha256sum --check - && \
  tar -C "$IMPORT_ROOT" -xzf /media/transfer/hyperfleet-mirror.tgz
export DESTINATION='mirror.example.com:8443'
export REGISTRY_AUTH_FILE='/path/to/destination-auth.json'
oc-mirror --v2 --authfile "$REGISTRY_AUTH_FILE" \
  --config "$IMPORT_ROOT/imageset-config.yaml" \
  --from file://"$IMPORT_ROOT/archive" docker://"$DESTINATION"
```

Do not extract the archive or use its `imageset-config.yaml` unless this
publisher-provided verification succeeds. Retain the verified archive digest
with the disconnected installation evidence.

Some registries limit repository nesting. Use `--max-nested-paths` if required
and supported by your selected oc-mirror version. Grant pull access to
the service accounts used by CatalogSource, OLM bundle unpack, manager and
operands, including cross-project access when images live in another project.
The import user's successful push does not grant those workloads pull access.

Apply the generated image-mirror resources and CatalogSource. The generated
file names may vary, so apply every matching IDMS, ITMS, and `cs-*.yaml` file:

```bash
export CLUSTER_RESOURCES="$IMPORT_ROOT/archive/working-dir/cluster-resources"
for manifest in "$CLUSTER_RESOURCES"/idms-*.yaml \
                "$CLUSTER_RESOURCES"/itms-*.yaml \
                "$CLUSTER_RESOURCES"/cs-*.yaml; do
  [ -e "$manifest" ] || continue
  oc apply -f "$manifest"
done
oc get catalogsource -n openshift-marketplace
```

Select the OLM v0 CatalogSource resources for this operator, not an OLM v1
ClusterCatalog.

Use the **generated** CatalogSource image reference: filtering can rebuild the
catalog, so its destination digest need not equal the source catalog digest.
Wait for registry configuration rollout and the CatalogSource connection state
to become `READY` before subscribing. Do not proceed on incomplete import or
missing image errors.

## 4. Install through OLM

Public registry access must be unavailable during an isolated acceptance test.
Verify the restriction at the node/container-runtime pull path, not only with
a namespace NetworkPolicy. Use fresh nodes or establish that the tested images
are not already cached; a new namespace alone does not do this. Preserve proof
of the restriction and the resulting mirror pulls.

Create a dedicated
namespace, an all-namespaces OperatorGroup, and a Subscription. Replace the
source name below with `metadata.name` from the generated CatalogSource.

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: hyperfleet-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: hyperfleet
  namespace: hyperfleet-system
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: hyperfleet-operator
  namespace: hyperfleet-system
spec:
  channel: stable
  name: hyperfleet-operator
  source: REPLACE_WITH_GENERATED_CATALOGSOURCE_NAME
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
```

Save the manifest as `hyperfleet-subscription.yaml`, apply it, and wait for OLM
to install the operator:

```bash
oc apply -f hyperfleet-subscription.yaml
oc get installplan,csv -n hyperfleet-system
```

With `installPlanApproval: Automatic`, OLM creates and approves the InstallPlan.
Wait for the installed CSV to reach `Succeeded`; do not hardcode a CSV version
from the development repository. If your cluster requires change control, use a
separate, manual-approval procedure: set `installPlanApproval: Manual`, inspect
the generated InstallPlan and its selected CSV, then approve that specific plan.
The bundle supports `AllNamespaces`; do not add `targetNamespaces` to this
OperatorGroup.

Create a valid HyperFleetConfig and its referenced Secrets, with reachable
database and authentication services as required by that configuration. Confirm
operator and API readiness and inspect pod events for failed pulls. Compare
runtime images with the selected release bundle; multi-architecture image IDs
may identify platform manifests beneath the declared image index.

For the release installed by this guide, provide the database Secret referenced
by `spec.api.database.secretRef.name`; database provisioning is outside this
installation procedure. Its keys are `db.host`, `db.port`, `db.name`, `db.user`
and `db.password`. Consult the API contract and installation documentation
shipped with the selected release for its database requirements.

Do not substitute `operator-sdk run bundle` for this installation path: its
development catalog helpers are not the mirrored release catalog.

Keep a sanitized installation record containing catalog and bundle digests,
package/channel, tool and cluster versions, export/import results, generated
mirror resources, isolation checks, and OLM/operand states. Never include
credentials. Successful archive transfer alone does not establish successful
installation or application readiness.

References: [oc-mirror filtering](https://github.com/openshift/oc-mirror/blob/main/docs/features/filtering.md),
[generated cluster resources](https://github.com/openshift/oc-mirror/blob/main/docs/features/cluster-resources.md).
