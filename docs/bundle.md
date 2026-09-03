# Bundle installation and development

For an OpenShift 4.18+ air-gapped installation using oc-mirror v2, follow the
[disconnected installation guide](disconnected-install.md). This document
covers bundle production and connected development workflows.

## Pre-merge checks
1. Updates to bundle.Dockerfile are also reflected in bundle.konflux.Dockerfile
2. bundle/ is correctly updated before merging
3. config/manager/kustomization.yaml is not wrongly updated

## CI Installation

Once Konflux is in place, the CI pipeline will automatically handle bundling building with operator image updates:

1. Konflux builds the operator image and publishes it to quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator
2. The operator-bundle .tekton pipeline will be triggered by any update to the bundle.konflux.Dockerfile
2. `bundle.konflux.Dockerfile` runs `hack/bundle/update_bundle.sh` with the new operator image reference
3. `hack/bundle/update_bundle.sh` uses yq to update the CSV to ensure the operator deployment has proper values - image, relatedImages, annotations, etc.
4. Publishes the operator-bundle to quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator-bundle

(HYPERFLEET-1411: TODO add more information once the konflux pipelines are in place)

## Development Installation

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


### OLM Installation
Testing hyperfleet-operator installation with OLM

**Note:** Ensure `IMG` is properly exported before running these commands

1. **Update bundle with operator image:** - WARNING restore changes once done testing!
   ```bash
   make bundle-override-img
   # Updates bundle/ manifests with the operator image from step 2
   # Alternative: manually edit config/manager/kustomization.yaml
   # Regenerates bundle.Dockerfile + bundle/ and override config/manager/kustomization.yaml
   ```

2. **Build and push bundle image:**
   ```bash
   make bundle-build
   make bundle-push
   # Pushes to: quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
   # To override: make bundle-build VERSION=0.0.2 BUNDLE_IMG=<another_repo>
   ```

3. **Quick testing on a k8s cluster:**
    ```bash
    export BUNDLE_IMG=quay.io/$QUAY_USER/hyperfleet-operator-bundle:v$(VERSION)
    # Install Operator Lifecycle Manager in your cluster
    operator-sdk olm install
    
    # Install operator from bundle (note: bundle image uses v prefix)
    operator-sdk run bundle $(BUNDLE_IMG) -n <NAMESPACE>
    
    # Cleanup when done - IMPORTANT: Delete CRs before uninstalling operator
    # 1. Export and delete the cluster-scoped HyperFleetConfig CR
    kubectl get hyperfleetconfig -o yaml > hyperfleetconfig-backup.yaml
    kubectl delete hyperfleetconfig --all

    # 2. Clean up operator (removes CRDs and controller)
    operator-sdk cleanup hyperfleet-operator -n <NAMESPACE>

    # 3. Uninstall Operator Lifecycle Manager from your cluster
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
    
    # Cleanup - IMPORTANT: Delete CRs before uninstalling operator
    # 1. Export and delete the cluster-scoped HyperFleetConfig CR
    kubectl get hyperfleetconfig -o yaml > hyperfleetconfig-backup.yaml
    kubectl delete hyperfleetconfig --all

    # 2. Undeploy operator (removes CRDs and controller)
    make undeploy
    # Or manually: kubectl delete -f dist/install.yaml
    ```

**Note:** `bundle-override-img` and `build-deployer-override-img` modify config/manager/kustomization.yaml in place. So before committing any changes make sure to revert these changes. Additionally when running `bundle-override-img` the bundle/ and bundle.Dockerfile get regenerated in place, so make sure to check these changes before committing them.
