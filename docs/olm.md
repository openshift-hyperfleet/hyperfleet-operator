# Hyperfleet-Operator Installation via OLM

## CI Image Build - Operator Image + Operator Bundle + Operator Catalog

Konflux uses a cascading build-nudge chain to keep operator, bundle, and catalog images in sync. Each stage triggers the next automatically:

```text
operator image ──nudge──▶ bundle image ──nudge──▶ catalog image
```

1. **Operator image build** — Konflux builds and releases the operator image to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator`.

2. **Nudge to bundle** — Konflux auto-merges the new operator image digest into `config/manifests/prod/kustomization.yaml`. This commit triggers the `operator-bundle-push` pipeline, which builds a new bundle image containing the updated operator reference.
   > Updates to `RELATED_IMAGE_HYPERFLEET_API` or `RELATED_IMAGE_HYPERFLEET_SENTINEL` also trigger this pipeline.

3. **Nudge to catalog** — Konflux auto-merges the new bundle image digest into `catalog/konflux-template.yaml`. This commit triggers the `operator-catalog-push` pipeline. Its `run-opm-command` task renders the template into `catalog/hyperfleet-operator/catalog.yaml` before the image build, so [catalog.Dockerfile](../catalog.Dockerfile) only copies the rendered catalog and never pulls the bundle image. The pipeline then builds the catalog image and publishes it to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator-catalog`.

> **Note**: Manual changes to [bundle.Dockerfile](../bundle.Dockerfile) or `config/manifests/prod/kustomization.yaml` will also trigger the bundle pipeline. Similarly, changes to [konflux-template.yaml](../catalog/konflux-template.yaml) or [catalog.Dockerfile](../catalog.Dockerfile) will trigger the catalog pipeline.

> **TODO** - HYPERFLEET-1617 - Update documentation based on release details for the catalog. Currently no upgrade graph for the hyperfleet-operator.

### Adding a component image to the nudge chain

`hyperfleet-api` and `hyperfleet-sentinel` builds reach the bundle through the same nudge as the operator image. Adding a new operand image requires changes in this repository and konflux-release-data.

1. **Prod overlay entry (this repository).** Wire `RELATED_IMAGE_HYPERFLEET_<COMPONENT>` through the code and overlays as [component pattern §8](component-pattern.md#8-add-the-image-and-related-image-plumbing) describes. In `config/manifests/prod/kustomization.yaml`, the entry's value is the component's services-prod image by digest, `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/<component>@sha256:<digest>`. Take the first digest from the component's latest released image: `skopeo inspect --override-os linux --override-arch amd64 docker://quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/<component>:latest --format '{{.Digest}}'`. The override flags let the command run on macOS and on multi-architecture images. The services-prod image exists only after the ReleasePlanAdmission in step 2 maps the component and a release has run. The nudge rewrites that digest, so the entry must exist before the first nudge. The tenant's `namespace-wide-nudging-renovate-config.yaml` in konflux-release-data (`fileMatch`) lets nudges edit only `catalog/konflux-template.yaml` and `config/manifests/prod/kustomization.yaml`, so keep the digest in that kustomization.

2. **Tenant nudge line (konflux-release-data).** In the GitLab repository `releng/konflux-release-data`, add `build-nudges-ref` to the component under `tenants-config/cluster/kflux-prd-rh02/tenants/hyperfleet-tenant/applications/hyperfleet/components/<component>.yaml`, matching `hyperfleet-api.yaml`:
   ```yaml
   spec:
     build-nudges-ref:
       - hyperfleet-operator-bundle
   ```
   Regenerate the manifests from `tenants-config/`, then commit the changes in both `cluster/` and `auto-generated/`:
   ```bash
   ./build-single.sh hyperfleet-tenant
   git diff -- cluster auto-generated   # the build-nudges-ref addition, in the component and in its generated manifest
   ```
   The script needs `kustomize` (on your `PATH`, or pass its path as the second argument) and mikefarah `yq`.

   The nudge carries the services-prod digest only if the `hyperfleet` application's ReleasePlanAdmission, `config/kflux-prd-rh02.0fk9.p1/service/ReleasePlanAdmission/hyperfleet/hyperfleet.yaml`, maps the component name to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/<component>`. Confirm the mapping is present and quote it in the merge request. If it is missing, add it: without the mapping the nudge does not update the services-prod reference.

Follow [Merge order for a new component image](#merge-order-for-a-new-component-image), then verify that a successful component build opens a nudge pull request and that the `hyperfleet-operator-bundle-on-push` run on its merge commit passes.

### Merge order for a new component image

Nudge pull requests come from `red-hat-konflux-kflux-prd-rh02[bot]` with the `konflux-nudge` label. The Prow tide query for this repository (`core-services/prow/02_config/openshift-hyperfleet/hyperfleet-operator/_prowconfig.yaml` in `openshift/release`) merges them once the presubmits pass. There is no presubmit (pull request) Konflux pipeline, so the push build after the merge is the first place to check that the bundle build succeeds.

Merge the two changes from [Adding a component image to the nudge chain](#adding-a-component-image-to-the-nudge-chain) in this order:

1. The operator-repository change merges. The bundle built from that merge commit already lists the component's digest, but it still pins the previous operator digest, and that operator doesn't deploy the component.
2. **Gate:** wait for the Konflux "Update hyperfleet-operator to …" nudge whose operator image was built from that merge commit or a later one, and let it merge. The title shows only a digest prefix; the first line of the pull request body names the source commit (`Image created from '…?rev=<sha>'`). To read it from the image instead: `skopeo inspect docker://quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator@sha256:<digest> --format '{{index .Labels "vcs-ref"}}'`. The bundle then pins an operator image that deploys the component.
3. The konflux-release-data merge request merges.
4. The component's next build opens an "Update `<component>` to …" pull request, which auto-merges and triggers a bundle build whose CSV carries the new digest.

Why the gate: the team rule is that a component nudges the bundle only if the operator can deploy it (HYPERFLEET-1665). Konflux does not check this; the merge order does. The nudge and the bundle build check image delivery, not runtime readiness. For that, follow the [disconnected installation guide](disconnected-install.md#4-install-through-olm).

## Developer Installation

### Prerequisites
- go version v1.26.0+
- docker version 18.06+
- operator-sdk v1.42.3+
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.27.0+ cluster.

### Prerequisite steps

The bundle includes both `HyperFleetConfig` and `ManagementClusterRegistration`
as owned APIs. OLM installs and upgrades their CRDs with the operator; no separate
registration bootstrap is required. The bundle also includes an unbound,
registration-only administrator role. OLM separately aggregates owned-API access
into generic `admin`, `edit` and `view` roles. Cluster-wide `admin`/`edit`
principals are trusted registration administrators; namespace-only RoleBindings
do not grant access to this cluster-scoped API. See
[registration installation and lifecycle](managementclusterregistration-reference.md#olm-installation-and-lifecycle)
for delegation and retention details.

For local development and installation, set your Quay username to automatically configure image paths:

1. **Export your quay username:** (Required)
   ```bash
   export QUAY_USER=<YOUR_QUAY_USERNAME>
   ```
2. **Build and push the operator image:**
   ```bash
   make image-dev OPERATOR_IMG=...
   # default OPERATOR_IMG=quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>
   ```
3. **Build the bundle image:**
   ```bash
   make bundle-build BUNDLE_IMG=... RELATED_IMAGE_HYPERFLEET_OPERATOR=... RELATED_IMAGE_HYPERFLEET_API=... RELATED_IMAGE_HYPERFLEET_SENTINEL=...
   # default BUNDLE_IMG=quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
   # default RELATED_IMAGE_HYPERFLEET_OPERATOR=quay.io/$QUAY_USER/hyperfleet-operator:$(IMG_TAG)
   # default RELATED_IMAGE_HYPERFLEET_API=quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api:latest
   # default RELATED_IMAGE_HYPERFLEET_SENTINEL=quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-sentinel:latest
   # default VERSION = 0.0.1
   ```
   `bundle-build` writes the operator image reference
   `quay.io/$QUAY_USER/hyperfleet-operator:$(IMG_TAG)` into the bundle. `IMG_TAG`
   defaults to `APP_VERSION` (`git describe --tags --always --dirty`). Only
   `image-dev` uses `dev-<git-sha>`. To bundle the image from step 2, pass the
   same reference as `RELATED_IMAGE_HYPERFLEET_OPERATOR`. If you built step 2
   with the default `OPERATOR_IMG`, `IMG_TAG=dev-$(git rev-parse --short HEAD)`
   resolves to the same reference.

5. **Push the bundle image:**
   ```bash
   make bundle-push BUNDLE_IMG=...
   ```

**Image path defaults with QUAY_USER set:**
- OPERATOR_IMG (hyperfleet-operator): `quay.io/$QUAY_USER/hyperfleet-operator:$(IMG_TAG)` (`IMG_TAG` defaults to `APP_VERSION`; `image-dev` uses `dev-<git-sha>`)
- BUNDLE_IMG (hyperfleet-operator-bundle): `quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)` (default VERSION=0.0.1)
- CATALOG_IMG (hyperfleet-operator-catalog): `quay.io/$QUAY_USER/hyperfleet-operator-catalog:v$(VERSION)` (defaul VERSION=0.0.1)


### Bundle + Catalog with OLM Classic V0
Testing hyperfleet-operator installation with OLM using a catalog image.

The catalog is defined by a single template, `catalog/konflux-template.yaml`, holding the package, channel and bundle entries. It is rendered before the image build, never inside `catalog.Dockerfile`: Konflux renders it in the pipeline, and locally `make catalog-render` renders it with the `olm.bundle` image swapped for `BUNDLE_IMG`. The rendered `catalog/hyperfleet-operator/catalog.yaml` is git-ignored.

**Note:** Ensure `QUAY_USER`, `BUNDLE_IMG` and `CATALOG_IMG` are set before running these commands
**Note:** Make sure you have completed the steps in [Prerequisite Steps](#prerequisite-steps)

1. **Render the catalog and build the catalog image:**
   ```bash
   make catalog-build BUNDLE_IMG=... CATALOG_IMG=...
   # runs make catalog-render first, which renders catalog/hyperfleet-operator/catalog.yaml
   # default BUNDLE_IMG=quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
   # default CATALOG_IMG=quay.io/$QUAY_USER/hyperfleet-operator-catalog:v$(VERSION)
   # default VERSION = 0.0.1
   ```
   The bundle image must already be pushed, rendering pulls it to read its metadata.

2. **Push the catalog image:**
   ```bash
   make catalog-push CATALOG_IMG=...
   ```

3. **Deploy on a k8s cluster with OLM:**
   ```bash
   # Install OLM if not already installed
   operator-sdk olm install

   # Create a CatalogSource
   kubectl apply -f - <<EOF
   apiVersion: operators.coreos.com/v1alpha1
   kind: CatalogSource
   metadata:
     name: hyperfleet-operator-catalog
     namespace: <NAMESPACE>
   spec:
     sourceType: grpc
     image: quay.io/$QUAY_USER/hyperfleet-operator-catalog:v$(VERSION)
     displayName: HyperFleet Operator
     publisher: Red Hat
     updateStrategy:
       registryPoll:
         interval: 5m
   EOF

   # Create an OperatorGroup (AllNamespaces mode)
   kubectl apply -f - <<EOF
   apiVersion: operators.coreos.com/v1
   kind: OperatorGroup
   metadata:
     name: hyperfleet-operator-group
     namespace: <NAMESPACE>
   spec: {}
   EOF

   # Create a Subscription
   kubectl apply -f - <<EOF
   apiVersion: operators.coreos.com/v1alpha1
   kind: Subscription
   metadata:
     name: hyperfleet-operator
     namespace: <NAMESPACE>
   spec:
     channel: stable
     name: hyperfleet-operator
     source: hyperfleet-operator-catalog
     sourceNamespace: <NAMESPACE>
     installPlanApproval: Automatic
   EOF

   # Watch the installation
   kubectl get sub,installplan,csv -n <NAMESPACE> -w
   ```

4. **Cleanup:**
    ```bash
    # IMPORTANT: Delete CRs before uninstalling operator
    kubectl get hyperfleetconfig -o yaml > hyperfleetconfig-backup.yaml
    kubectl delete hyperfleetconfig --all

    # Subscription and CSV deletion retains CRDs and their records. Back up
    # registrations first; any failure stops the sequence before the teardown
    # below runs.
    kubectl get managementclusterregistrations -o yaml > managementclusterregistrations-backup.tmp &&
      mv managementclusterregistrations-backup.tmp managementclusterregistrations-backup.yaml &&
      kubectl delete sub hyperfleet-operator -n <NAMESPACE> --ignore-not-found &&
      kubectl delete csv hyperfleet-operator.v0.0.1 -n <NAMESPACE> --ignore-not-found &&
      kubectl delete catalogsource hyperfleet-operator-catalog -n <NAMESPACE> --ignore-not-found &&
      kubectl delete operatorgroup hyperfleet-operator-group -n <NAMESPACE> --ignore-not-found &&
      operator-sdk olm uninstall
    ```

> **TODO**: HYPERFLEET-1616: OLM V1 install

### Install with operator-sdk and bundle - (operator-sdk run bundle)
Quick testing with `operator-sdk run bundle` (no catalog needed).

**Note:** Ensure `BUNDLE_IMG` is exported before running these commands

3. **Quick testing on a k8s cluster:**
    ```bash
    # Install Operator Lifecycle Manager in your cluster
    operator-sdk olm install

    # Install hyperfleet-operator from bundle
    operator-sdk run bundle $BUNDLE_IMG -n <NAMESPACE>

    # Apply an example hyperfleetconfig cr
    kubectl apply -k config/samples/

    # Cleanup when done - IMPORTANT: Delete CRs before uninstalling operator
    kubectl delete hyperfleetconfig --all

    # operator-sdk cleanup deletes the CRDs the bundle owns, including the
    # registration CRD, and destroys every registration record with it.
    # Back up first; any failure stops the sequence before cleanup and the OLM
    # uninstall.
    kubectl get managementclusterregistrations -o yaml > managementclusterregistrations-backup.tmp &&
      mv managementclusterregistrations-backup.tmp managementclusterregistrations-backup.yaml &&
      operator-sdk cleanup hyperfleet-operator -n <NAMESPACE> &&
      operator-sdk olm uninstall

    # If cleanup succeeded but the OLM uninstall failed, rerun only
    # `operator-sdk olm uninstall`: the registration CRD is already gone, so a
    # rerun stops at the backup step above.
    ```
