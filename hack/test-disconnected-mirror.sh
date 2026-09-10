#!/usr/bin/env bash
set -euo pipefail

# Catalog archive transfer test. This does not install the operator on a cluster.
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
OC_MIRROR_IMAGE="${OC_MIRROR_IMAGE:-hyperfleet-oc-mirror:local}"
# Disposable destination registry for the isolated disk-to-mirror phase.
REGISTRY_IMAGE="${REGISTRY_IMAGE:-docker.io/library/registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373}"
YQ="${YQ:-yq}"
KEEP_WORKSPACE="${KEEP_WORKSPACE:-false}"
CONTAINER_DNS="${CONTAINER_DNS:-}"
MIRROR_PLATFORM="${MIRROR_PLATFORM:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail() { echo "error: $*" >&2; exit 1; }

if [[ -n "$MIRROR_PLATFORM" ]]; then
  [[ "$MIRROR_PLATFORM" =~ ^linux/(amd64|arm64)$ ]] ||
    fail "MIRROR_PLATFORM must be linux/amd64 or linux/arm64, got $MIRROR_PLATFORM"
fi

container_with_platform() {
  local subcommand="$1"
  shift

  if [[ -n "$MIRROR_PLATFORM" ]]; then
    "$CONTAINER_TOOL" "$subcommand" --platform "$MIRROR_PLATFORM" "$@"
  else
    "$CONTAINER_TOOL" "$subcommand" "$@"
  fi
}

destination_for() {
  local image="$1"
  local repository="${image%@sha256:*}"
  local digest="@sha256:${image##*@sha256:}"
  local source mirror matched_source="" matched_mirror=""

  while IFS=$'\t' read -r source mirror; do
    if [[ "$repository" == "$source" || "$repository" == "$source/"* ]] &&
      (( ${#source} > ${#matched_source} )); then
      matched_source="$source"
      matched_mirror="$mirror"
    fi
  done < <("$YQ" eval -r '
    .spec.imageDigestMirrors[] | [.source, .mirrors[0]] | @tsv
  ' "$resources/idms-oc-mirror.yaml")

  [[ -n "$matched_source" ]] || return 1
  printf '%s%s%s\n' "$matched_mirror" "${repository#"$matched_source"}" "$digest"
}

# connected_docker_run permits an explicit resolver only for the container that
# reads the connected source registry. The disconnected import must retain
# Docker's embedded DNS so it can resolve the disposable registry alias.
connected_docker_run() {
  if [[ -n "$CONTAINER_DNS" ]]; then
    container_with_platform run --rm --dns "$CONTAINER_DNS" "$@"
  else
    container_with_platform run --rm "$@"
  fi
}

verify_destination_image() {
  local image="$1"
  local platform_description="the runner platform"
  local inspect_args=(inspect)

  if [[ -n "$MIRROR_PLATFORM" ]]; then
    platform_description="$MIRROR_PLATFORM"
    inspect_args+=(
      --override-os "${MIRROR_PLATFORM%%/*}"
      --override-arch "${MIRROR_PLATFORM#*/}"
    )
  fi

  # The raw manifest establishes that the archive transfer retained the image.
  container_with_platform run --rm --network "$network" \
    --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" \
    inspect --raw --tls-verify=false "docker://$image" >/dev/null

  # Raw presence is insufficient for a CatalogSource or bundle image: the
  # target platform must be able to select a runnable manifest from its index.
  container_with_platform run --rm --network "$network" \
    --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" \
    "${inspect_args[@]}" --tls-verify=false "docker://$image" >/dev/null ||
    fail "destination image cannot be selected for $platform_description: $image"
}

for tool in "$CONTAINER_TOOL" "$YQ"; do
  command -v "$tool" >/dev/null 2>&1 || fail "required command: $tool"
done
[[ "${CATALOG_IMG:-}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]] ||
  fail "set CATALOG_IMG to the digest-pinned catalog to test"
for image in "$OC_MIRROR_IMAGE" "$REGISTRY_IMAGE"; do
  "$CONTAINER_TOOL" image inspect "$image" >/dev/null 2>&1 ||
    fail "prepare test helper image on the connected host first: $image"
done

workspace="$(mktemp -d "${TMPDIR:-/tmp}/hyperfleet-catalog-mirror.XXXXXX")"
connected="$workspace/connected"
disconnected="$workspace/disconnected"
mkdir -p "$connected/home" "$disconnected/home" "$disconnected/archive"
network="hyperfleet-catalog-$$"
registry_container=""
catalog_container=""
cleanup() {
  # Never retain source registry credentials in an evidence workspace, even
  # when the workspace is kept after a failure.
  rm -f "$connected/auth.json"
  [[ -z "$catalog_container" ]] || "$CONTAINER_TOOL" rm -f "$catalog_container" >/dev/null 2>&1 || true
  [[ -z "$registry_container" ]] || "$CONTAINER_TOOL" rm -f "$registry_container" >/dev/null 2>&1 || true
  "$CONTAINER_TOOL" network rm "$network" >/dev/null 2>&1 || true
  if [[ "$KEEP_WORKSPACE" == true ]]; then
    echo "evidence workspace: $workspace"
  else
    rm -rf "$workspace"
  fi
}
trap cleanup EXIT

# The catalog is the sole release input. Read its selected bundle and images to
# verify the imported registry without reading repository manifests or accepting
# a separately supplied bundle image.
"$YQ" eval '.mirror.operators[0].catalog = strenv(CATALOG_IMG)' \
  "$SCRIPT_DIR/../docs/examples/imageset-config-catalog.yaml" >"$connected/imageset-config.yaml"
package_name="$("$YQ" eval -r '.mirror.operators[0].packages[0].name' "$connected/imageset-config.yaml")"
channel_name="$("$YQ" eval -r '.mirror.operators[0].packages[0].channels[0].name' "$connected/imageset-config.yaml")"
[[ -n "$package_name" && "$package_name" != null && -n "$channel_name" && "$channel_name" != null ]] ||
  fail "ImageSetConfiguration must select one package and channel"

# A file-based catalog exposes the FBC directory through this standard label.
# Pulling the immutable catalog is a connected-host operation and uses the
# container engine's registry login; oc-mirror has its own optional auth file.
container_with_platform pull "$CATALOG_IMG" >/dev/null
catalog_configs_path="$("$CONTAINER_TOOL" image inspect "$CATALOG_IMG" \
  --format '{{ index .Config.Labels "operators.operatorframework.io.index.configs.v1" }}')"
[[ "$catalog_configs_path" == /* ]] ||
  fail "catalog is not a file-based catalog with an index configs label"
mkdir -p "$workspace/catalog-configs"
catalog_container="$(container_with_platform create --pull=never --entrypoint /bin/true "$CATALOG_IMG")"
"$CONTAINER_TOOL" cp "$catalog_container:$catalog_configs_path/." "$workspace/catalog-configs"
"$CONTAINER_TOOL" rm "$catalog_container" >/dev/null
catalog_container=""

catalog_files=()
while IFS= read -r -d '' file; do
  catalog_files+=("$file")
done < <(find "$workspace/catalog-configs" -type f \( -name '*.yaml' -o -name '*.yml' -o -name '*.json' \) -print0)
(( ${#catalog_files[@]} > 0 )) || fail "catalog has no FBC metadata files"

channel_entry_names=()
replaced_bundle_names=()
while IFS=$'\t' read -r bundle_name replaces; do
  [[ -n "$bundle_name" ]] || continue
  channel_entry_names+=("$bundle_name")
  [[ -z "$replaces" || "$replaces" == null ]] || replaced_bundle_names+=("$replaces")
done < <(PACKAGE_NAME="$package_name" CHANNEL_NAME="$channel_name" "$YQ" eval-all -r '
  select(.schema == "olm.channel" and .package == strenv(PACKAGE_NAME) and .name == strenv(CHANNEL_NAME)) |
  .entries[] | [.name, (.replaces // "")] | @tsv
' "${catalog_files[@]}")
channel_heads=()
for bundle_name in "${channel_entry_names[@]}"; do
  is_replaced=false
  for replaced_name in "${replaced_bundle_names[@]-}"; do
    if [[ "$bundle_name" == "$replaced_name" ]]; then
      is_replaced=true
      break
    fi
  done
  [[ "$is_replaced" == true ]] || channel_heads+=("$bundle_name")
done
(( ${#channel_heads[@]} == 1 )) ||
  fail "could not identify one head bundle for $package_name/$channel_name"

expected_images=()
while IFS= read -r image; do
  [[ -n "$image" ]] && expected_images+=("$image")
done < <(OLM_BUNDLE_NAME="${channel_heads[0]}" "$YQ" eval-all -r '
  select(.schema == "olm.bundle" and .name == strenv(OLM_BUNDLE_NAME)) |
  (.image, .relatedImages[]?.image)
' "${catalog_files[@]}")
(( ${#expected_images[@]} > 0 )) ||
  fail "catalog head bundle ${channel_heads[0]} has no images"
for image in "${expected_images[@]}"; do
  [[ "$image" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]] ||
    fail "catalog bundle contains a non-digest image: $image"
done

mirror_args=(--v2 --config imageset-config.yaml file://archive)
if [[ -f "${REGISTRY_AUTH_FILE:-}" ]]; then
  if "$YQ" -e '.credsStore? or .credHelpers?' "$REGISTRY_AUTH_FILE" >/dev/null 2>&1; then
    fail "REGISTRY_AUTH_FILE uses a Docker credential helper; provide a containers auth file with inline auth entries"
  fi
  cp "$REGISTRY_AUTH_FILE" "$connected/auth.json"
  mirror_args=(--authfile /work/auth.json "${mirror_args[@]}")
fi
# oc-mirror discovers bundles and related images from the selected catalog.
connected_docker_run \
  -e HOME=/work/home -v "$connected:/work:z" -w /work \
  "$OC_MIRROR_IMAGE" "${mirror_args[@]}"
# The auth file is needed only by the connected mirror operation. Remove it
# before any evidence workspace can be retained; cleanup also covers failures.
rm -f "$connected/auth.json"

archives=("$connected"/archive/mirror_*.tar)
[[ -e "${archives[0]}" ]] || fail "mirror-to-disk produced no archives"
for archive in "${archives[@]}"; do
  cp "$archive" "$disconnected/archive/"
done
cp "$connected/imageset-config.yaml" "$disconnected/"

# The import gets only archives and configuration, without connected caches or
# source credentials. Its registry and runner share a network without egress.
if [[ "$CONTAINER_TOOL" == podman ]]; then
  podman_network_backend="$("$CONTAINER_TOOL" info --format '{{.Host.NetworkBackend}}')" ||
    fail "could not determine the Podman network backend"
  [[ "$podman_network_backend" != cni ]] ||
    fail "Podman CNI networking is unsupported: use netavark so the registry network alias resolves"
fi
"$CONTAINER_TOOL" network create --internal "$network" >/dev/null
registry_container="$(container_with_platform run -d --rm --pull=never --network "$network" \
  --network-alias registry "$REGISTRY_IMAGE")"

registry_ready=false
registry_probe_output=""
for ((attempt = 1; attempt <= 30; attempt++)); do
  registry_probe_output="$(container_with_platform run --rm --pull=never --network "$network" \
    --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" \
    list-tags --tls-verify=false docker://registry:5000/readiness 2>&1)" || true
  # A fresh registry returns NAME_UNKNOWN for this deliberately absent
  # repository. That response proves registry:5000 accepted the request.
  if [[ "$registry_probe_output" == *"repository name not known to registry"* ]]; then
    registry_ready=true
    break
  fi
  sleep 1
done
[[ "$registry_ready" == true ]] ||
  fail "registry:5000 was not ready after 30 seconds: $registry_probe_output"

# Check Docker Hub connectivity from the isolated runner. The internal network
# provides isolation; this probe alone does not test every public registry.
# Verify the pinned runner image and its documented skopeo entrypoint first so
# missing images, missing skopeo, and container startup errors fail the test.
skopeo_version="$(container_with_platform run --rm --pull=never --network "$network" \
  --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" --version 2>&1)" ||
  fail "isolated runner could not execute /usr/bin/skopeo: $skopeo_version"
[[ "$skopeo_version" == skopeo\ version\ * ]] ||
  fail "unexpected skopeo version output: $skopeo_version"

if isolation_error="$(container_with_platform run --rm --pull=never --network "$network" \
  --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" \
  inspect docker://docker.io/library/registry:2 2>&1)"; then
  fail "disconnected network unexpectedly reached Docker Hub"
fi
# Accept only a Docker Hub DNS or connection failure. Do not accept unrelated
# errors, such as an absent image or a failed container startup.
[[ "$isolation_error" == *"registry-1.docker.io"* ]] ||
  fail "unexpected Docker Hub isolation error: $isolation_error"
[[ "$isolation_error" =~ (dial\ tcp|no\ such\ host|server\ misbehaving|network\ is\ unreachable|i/o\ timeout) ]] ||
  fail "unexpected Docker Hub isolation error: $isolation_error"

container_with_platform run --rm --network "$network" \
  -e HOME=/work/home -v "$disconnected:/work:z" -w /work \
  "$OC_MIRROR_IMAGE" --v2 --config imageset-config.yaml \
  --from file://archive --dest-tls-verify=false docker://registry:5000

resources="$disconnected/archive/working-dir/cluster-resources"
[[ -f "$resources/idms-oc-mirror.yaml" ]] || fail "import did not generate digest mirror mappings"
catalogs=("$resources"/cs-*.yaml)
[[ -e "${catalogs[0]}" ]] || fail "import did not generate a CatalogSource"
for manifest in "${catalogs[@]}"; do
  target="$("$YQ" eval '.spec.image' "$manifest")"
  [[ "$target" == registry:5000/* ]] || fail "CatalogSource does not use destination registry: $target"
  verify_destination_image "$target"
done
for image in "${expected_images[@]}"; do
  destination="$(destination_for "$image")" ||
    fail "generated IDMS does not cover catalog-selected image: $image"
  verify_destination_image "$destination"
done
echo "catalog head bundle and all related images are present after archive import"
echo "OpenShift installation and operand readiness have NOT been tested"
