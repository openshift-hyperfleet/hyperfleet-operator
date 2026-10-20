# Hyperfleet-Operator Installation via OLM

## CI Image Build - Operator Image + Operator Bundle + Operator Catalog

Konflux uses a cascading build-nudge chain to keep operator, bundle, and catalog images in sync. Each stage triggers the next automatically:

```
operator image ──nudge──▶ bundle image ──nudge──▶ catalog image
```

1. **Operator image build** — Konflux builds and releases the operator image to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator`.

2. **Nudge to bundle** — Konflux auto-merges the new operator image digest into `config/manifests/prod/kustomization.yaml`. This commit triggers the `operator-bundle-push` pipeline, which builds a new bundle image containing the updated operator reference.
   > Updates to `RELATED_IMAGE_HYPERFLEET_API` also trigger this pipeline.

3. **Nudge to catalog** — Konflux auto-merges the new bundle image digest into `catalog/konflux-template.yaml`. This commit triggers the `operator-catalog-push` pipeline. Its `run-opm-command` task renders the template into `catalog/hyperfleet-operator/catalog.yaml` before the image build, so [catalog.Dockerfile](../catalog.Dockerfile) only copies the rendered catalog and never pulls the bundle image. The pipeline then builds the catalog image and publishes it to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator-catalog`.

> **Note**: Manual changes to [bundle.Dockerfile](../bundle.Dockerfile) or `config/manifests/prod/kustomization.yaml` will also trigger the bundle pipeline. Similarly, changes to [konflux-template.yaml](../catalog/konflux-template.yaml) or [catalog.Dockerfile](../catalog.Dockerfile) will trigger the catalog pipeline.

> **TODO** - HYPERFLEET-1617 - Update documentation based on release details for the catalog. Currently no upgrade graph for the hyperfleet-operator.

## Developer Installation

### Prerequisites
- go version v1.26.0+
- docker version 18.06+
- operator-sdk v1.42.3+
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.27.0+ cluster.

### Prerequisite steps

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
   make bundle-build BUNDLE_IMG=... RELATED_IMAGE_HYPERFLEET_OPERATOR=... RELATED_IMAGE_HYPERFLEET_API=...
   # default BUNDLE_IMG=quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
   # default RELATED_IMAGE_HYPERFLEET_OPERATOR=quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>
   # default RELATED_IMAGE_HYPERFLEET_API=quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api:latest
   # default VERSION = 0.0.1
   ```
5. **Push the bundle image:**
   ```bash
   make bundle-push BUNDLE_IMG=...
   ```

**Image path defaults with QUAY_USER set:**
- OPERATOR_IMG (hyperfleet-operator): `quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>`
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

    kubectl delete sub hyperfleet-operator -n <NAMESPACE>
    kubectl delete csv hyperfleet-operator.v0.0.1 -n <NAMESPACE>
    kubectl delete catalogsource hyperfleet-operator-catalog -n <NAMESPACE>
    kubectl delete operatorgroup hyperfleet-operator-group -n <NAMESPACE>

    # Uninstall OLM from your cluster
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

    # Uninstall hyperfleet-operator
    operator-sdk cleanup hyperfleet-operator -n <NAMESPACE>
    # Uninstall olm
    operator-sdk olm uninstall
    ```
