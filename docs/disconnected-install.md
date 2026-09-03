# Disconnected mirroring with oc-mirror v2

This is the minimal disk-to-registry procedure for the HyperFleet Operator.
Use a released bundle, operator image, and API image identified by immutable
`@sha256:<64 lowercase hex characters>` pullspecs.

## Prerequisites

* `oc-mirror` v2 on the connected and disconnected hosts
* read access to the source registries
* write access to the disconnected registry
* transfer media with enough space for the archive
* an auth file and registry trust configured on both hosts

Do not put credentials in the configuration file:

```bash
export REGISTRY_AUTH_FILE=$HOME/.config/containers/auth.json
```

## 1. Create one ImageSetConfiguration

Set up the mirror workspace and copy [`docs/examples/imageset-config-standalone.yaml`](examples/imageset-config-standalone.yaml):

```bash
export MIRROR_ROOT="$HOME/hyperfleet-mirror"
rm -rf "$MIRROR_ROOT"
mkdir -p "$MIRROR_ROOT"
cp docs/examples/imageset-config-standalone.yaml "$MIRROR_ROOT/imageset-config.yaml"
```

Edit `$MIRROR_ROOT/imageset-config.yaml` to replace its three example pullspecs
with the released bundle, operator, and API image digests. The file uses
`additionalImages` intentionally: it transfers exactly the three listed
artifacts and does not discover a catalog or CSV.

Run the image check before mirroring:

```bash
make verify-related-images
```

It verifies this equality:

```text
deployable-images.yaml == CSV manager image + RELATED_IMAGE_* values == CSV spec.relatedImages
```

## 2. Mirror to disk on the connected host

```bash
oc-mirror --v2 \
  --config "$MIRROR_ROOT/imageset-config.yaml" \
  file://"$MIRROR_ROOT/archive"
```

Keep the complete `archive` directory. Transfer it and the configuration to the
disconnected mirroring host using approved media:

```bash
tar -C "$MIRROR_ROOT" \
  -czf /media/transfer/hyperfleet-mirror.tgz \
  archive imageset-config.yaml

mkdir -p /var/tmp/hyperfleet-mirror
tar -C /var/tmp/hyperfleet-mirror \
  -xzf /media/transfer/hyperfleet-mirror.tgz
```

## 3. Mirror from disk into the disconnected registry

```bash
export DESTINATION='mirror.example.com:8443/hyperfleet'

oc-mirror --v2 \
  --config /var/tmp/hyperfleet-mirror/imageset-config.yaml \
  --from file:///var/tmp/hyperfleet-mirror/archive \
  docker://"$DESTINATION"
```

Save the command output. Apply any generated mirror resources to the cluster
before installing the bundle, for example:

```bash
CLUSTER_RESOURCES="$(find /var/tmp/hyperfleet-mirror \
  -type d -path '*/working-dir/cluster-resources' | head -1)"
oc apply -f "$CLUSTER_RESOURCES"
```

Use the mirrored bundle with the normal installation procedure. The
`additionalImages` configuration does not create a catalog. Do not manually
rewrite image digests.

## Acceptance evidence

Exercise the connected disk export and disconnected import procedure once on an
isolated OpenShift cluster. Record the command output in the PR Test Plan,
including the cluster and OpenShift version, destination registry, mirrored
bundle digest, successful CSV/operator/API readiness, and runtime image IDs
ending in the expected digests. Do not include credentials or Secret values.

For a repeatable local transfer check, use the containerized oc-mirror runner:

```bash
BUNDLE_IMAGE='quay.io/.../hyperfleet-operator-bundle@sha256:<bundle-digest>' \
make test-disconnected-mirror
```

The runner is built from [`hack/oc-mirror.Dockerfile`](../hack/oc-mirror.Dockerfile)
and deliberately keeps oc-mirror in a container. This check exercises archive
transfer only; it is not a cluster installation framework.
