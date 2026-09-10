## Pre-merge checks
1. Updates to bundle.Dockerfile are also reflected in bundle.konflux.Dockerfile
2. bundle/ is correctly updated before merging
3. config/manager/kustomization.yaml is not wrongly updated

## CI Image Build - Operator Image + Operator Bundle + Operator Catalog

Konflux Workflow:

1. Konflux builds the operator image and publishes it to quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator
2. The update of the hyperfleet-operator image will trigger an update to bundle.konflux.Dockerfile. Konflux will create a PR for us and automerge the update.
3. The operator-bundle-push .tekton pipeline will be triggered by any update to the bundle.konflux.Dockerfile in main. So this new operator image update will trigger the build once merged into main. Note - `bundle.konflux.Dockerfile` runs `update_bundle.sh` with the new operator image reference. `update_bundle.sh` uses yq to update the CSV to ensure the operator deployment has proper values - image, relatedImages, annotations, etc. Once completed the pipeline publishes the operator-bundle to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator-bundle`
5. Subsequently, the operator-bundle-push pipeline will update the operator-bundle image in the `konflux-template.yaml`.
6. Konflux takes care of auto-merging the update, once merged, it will trigger the operator-catalog-push .tekton pipeline which will build operator-catalog image and push it to `quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator-catalog`

** TODO - HYPERFLEET-1617 - Update documentation based on release details for the catalog. Assumption right now is we will release the hyperfleet-operator as a catalog that can be installed with olm.

** Note: Any update to [bundle.konflux.Dockerfile](../bundle.konflux.Dockerfile) operator-bundle-push pipeline and any update to [konflux-template.yaml](../catalog/konflux-template.yaml) will trigger the operator-catalog-push pipeline.

## Developer Installation

### Tool Prerequisites
- go version v1.26.0+
- docker version 17.05+.
- kubectl version v1.11.3+.
- Access to a Kubernetes v1.11.3+ cluster.

### Prerequisite steps
For local development and installation, set your Quay username to automatically configure image paths:

```bash
# Set your Quay username (required for image-dev)
export QUAY_USER=<YOUR_QUAY_USERNAME>
# Checkout dev branch
git checkout -b <dev-branch>

# Build and push dev image
make image-dev
# With default values - pushes to: quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>
export IMG=quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>
# export IMG so that it can be properly picked up for bundle generation
```

**Image path defaults:**
- IMG (hyperfleet-operator image): `quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>` (defaults `make image-dev`)
- BUNDLE_IMG (hyperfleet-operator-bundle): `quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)` (default VERSION=0.0.1)
- CATALOG_IMG (hyperfleet-operator-catalog): `quay.io/$QUAY_USER/hyperfleet-operator-catalog:v$(VERSION)` (defaul VERSION=0.0.1)

### OLM Installation (Bundle + Catalog) - OLM Classic V0
Testing hyperfleet-operator installation with OLM using a catalog image.

The catalog build uses a template system with a base template (`catalog/base-template.yaml`) that defines the package and channel, and environment-specific templates that specify the bundle image:
- `catalog/dev-template.yaml` - for local development (default)
- `catalog/konflux-template.yaml` - for Konflux CI builds

**Note:** Ensure `IMG`, `BUNDLE_IMG` and `CATALOG_IMG` is properly exported before running these commands

1. **Update bundle with operator image:** - WARNING restore changes once done testing!
   ```bash
   make bundle-override-img
   # Updates bundle/ manifests with the operator image
   # Regenerates bundle.Dockerfile + bundle/ and overrides config/manager/kustomization.yaml
   ```

2. **Build and push bundle image:**
   ```bash
   make bundle-build bundle-push PLATFORM=linux/amd64
   # Pushes to: quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
   ```

3. **Update the catalog template with the new bundle image:**
   ```bash
   make catalog-template-update-bundle-img
   # Updates catalog/dev-template.yaml with the current BUNDLE_IMG
   ```

4. **Build and push the catalog image:**
   ```bash
   make catalog-build catalog-push PLATFORM=linux/amd64
   # Pushes to: quay.io/$QUAY_USER/hyperfleet-operator-catalog:v$(VERSION)
   # To use a different template: make catalog-build TEMPLATEFILE=konflux-template.yaml
   ```

5. **Deploy on a k8s cluster with OLM:**
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

6. **Cleanup:**
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

### OLM Installation (operator-sdk run bundle)
Quick testing with `operator-sdk run bundle` (no catalog needed).

**Note:** Ensure `IMG` and `BUNDLE_IMG` is properly exported before running these commands

1. **Update bundle with operator image:** - WARNING restore changes once done testing!
   ```bash
   make bundle-override-img
   ```

2. **Build and push bundle image:**
   ```bash
   make bundle-build bundle-push PLATFORM=linux/amd64
   ```

3. **Quick testing on a k8s cluster:**
    ```bash
    # Install Operator Lifecycle Manager in your cluster
    operator-sdk olm install
    
    # Install operator from bundle
    operator-sdk run bundle $BUNDLE_IMG -n <NAMESPACE>

    kubectl apply -k config/samples/
    
    # Cleanup when done - IMPORTANT: Delete CRs before uninstalling operator
    kubectl delete hyperfleetconfig --all

    operator-sdk cleanup hyperfleet-operator -n <NAMESPACE>
    operator-sdk olm uninstall
    ```


### Non-OLM Installation
Testing hyperfleet-operator installation without OLM (kubectl apply)

**Note:** Ensure `IMG` is properly exported before running these commands
1. **Quick testing on a k8s cluster:**
    ```bash
    export IMG="quay.io/$QUAY_USER/hyperfleet-operator:dev-<git-sha>"
    make deploy
    # Generates: dist/install.yaml
    # Again, make sure to restore config/manager/kustomization.yaml after testing
    # Check status to see that everything installed properly
    
    # Apply an example hyperfleetconfig CR
    kubectl apply -k config/samples/

    # Cleanup - IMPORTANT: Delete CRs before uninstalling operator
    # 1. Export and delete the cluster-scoped HyperFleetConfig CR
    kubectl get hyperfleetconfig -o yaml > hyperfleetconfig-backup.yaml
    kubectl delete hyperfleetconfig --all

    # 2. Undeploy operator (removes CRDs and controller)
    make undeploy
    # Or manually: kubectl delete -f dist/install.yaml
    ```

**Note:** `bundle-override-img` and `build-deployer-override-img` modify config/manager/kustomization.yaml in place. So before committing any changes make sure to revert these changes. Additionally when running `bundle-override-img` the bundle/ and bundle.Dockerfile get regenerated in place, so make sure to check these changes before committing them.
