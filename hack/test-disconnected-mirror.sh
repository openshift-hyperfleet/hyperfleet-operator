#!/usr/bin/env bash
set -euo pipefail

# Exercise oc-mirror v2's mirror-to-disk and disk-to-mirror path in containers.
# The archive is copied to a fresh workspace before the disconnected phase.

CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
OC_MIRROR_IMAGE="${OC_MIRROR_IMAGE:-hyperfleet-oc-mirror:local}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-docker.io/library/registry:2}"
KEEP_WORKSPACE="${KEEP_WORKSPACE:-false}"

fail() {
  echo "error: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command '$1' is not installed"
}

normalize_pullspec() {
  local name="$1"
  local image="${!name:-}"
  image="${image#https://}"
  image="${image#http://}"
  image="${image#docker://}"
  if [[ "$image" =~ :sha256-([0-9a-f]{64})$ ]]; then
    image="${image%:sha256-*}@sha256:${BASH_REMATCH[1]}"
  fi
  printf -v "$name" '%s' "$image"
}

require_image() {
  local name="$1"
  normalize_pullspec "$name"
  local image="${!name:-}"
  [[ "$image" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]] || \
    fail "$name must be set to a sha256 digest pullspec"
}

require_command "$CONTAINER_TOOL"

OPERATOR_IMAGE="${OPERATOR_IMAGE:-quay.io/redhat-user-workloads/hyperfleet-tenant/hyperfleet/hyperfleet-operator@sha256:31e365d312b6f3d483913d4029eba565abd64ab38a6d117658324afe225f708d}"
API_IMAGE="${API_IMAGE:-quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api@sha256:8533d0d875480f31f5112e454659a095a5d2e993c139a9045a06be6b67b829ca}"
BUNDLE_IMAGE="${BUNDLE_IMAGE:-}"
require_image BUNDLE_IMAGE
require_image OPERATOR_IMAGE
require_image API_IMAGE

if ! "$CONTAINER_TOOL" image inspect "$OC_MIRROR_IMAGE" >/dev/null 2>&1; then
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  echo "building $OC_MIRROR_IMAGE"
  "$CONTAINER_TOOL" build -f "$script_dir/oc-mirror.Dockerfile" -t "$OC_MIRROR_IMAGE" "$script_dir/.."
fi

workspace="$(mktemp -d "${TMPDIR:-/tmp}/hyperfleet-disconnected-mirror.XXXXXX")"
connected="$workspace/connected"
disconnected="$workspace/disconnected"
mkdir -p "$connected/home" "$disconnected/home" "$disconnected/archive"
network="hyperfleet-disconnected-$$"
registry_container=""

cleanup() {
  [[ -z "$registry_container" ]] || "$CONTAINER_TOOL" rm -f "$registry_container" >/dev/null 2>&1 || true
  "$CONTAINER_TOOL" network rm "$network" >/dev/null 2>&1 || true
  if [[ "$KEEP_WORKSPACE" == true ]]; then
    echo "workspace preserved at $workspace"
  else
    rm -rf "$workspace"
  fi
}
trap cleanup EXIT

find_auth_file() {
  if [[ -n "${REGISTRY_AUTH_FILE:-}" && -f "$REGISTRY_AUTH_FILE" ]]; then
    printf '%s\n' "$REGISTRY_AUTH_FILE"
    return
  fi
  local candidate
  for candidate in \
    "${XDG_RUNTIME_DIR:-}/containers/auth.json" \
    "${HOME}/.docker/config.json" \
    "${HOME}/.config/containers/auth.json"; do
    [[ -f "$candidate" ]] && { printf '%s\n' "$candidate"; return; }
  done
  return 1
}

auth_file="$(find_auth_file || true)"
if [[ -n "$auth_file" ]]; then
  cp "$auth_file" "$connected/auth.json"
  cp "$auth_file" "$disconnected/auth.json"
fi

cat >"$connected/imageset-config.yaml" <<EOF
apiVersion: mirror.openshift.io/v2alpha1
kind: ImageSetConfiguration
mirror:
  additionalImages:
  - name: $BUNDLE_IMAGE
  - name: $OPERATOR_IMAGE
  - name: $API_IMAGE
EOF

auth_args=()
[[ -z "$auth_file" ]] || auth_args+=(--authfile /work/auth.json)

# Connected phase. The container gets its normal network so it can read source
# registries. Only the resulting tar archives cross the archive boundary.
echo "running mirror-to-disk"
"$CONTAINER_TOOL" run --rm \
  -e HOME=/work/home -v "$connected:/work:z" -w /work \
  "$OC_MIRROR_IMAGE" "${auth_args[@]}" --v2 \
  --config imageset-config.yaml file://archive

archives=("$connected"/archive/mirror_*.tar)
[[ -e "${archives[0]}" ]] || fail "mirror-to-disk produced no mirror_*.tar archive"
for archive in "${archives[@]}"; do
  cp "$archive" "$disconnected/archive/"
done
cp "$connected/imageset-config.yaml" "$disconnected/"

[[ -z "${DESTINATION:-}" ]] || fail "this isolated test uses its disposable registry; unset DESTINATION"
"$CONTAINER_TOOL" network create --internal "$network" >/dev/null
registry_container="$($CONTAINER_TOOL run -d --rm --network "$network" --network-alias registry "$REGISTRY_IMAGE")"
DESTINATION="registry:5000/hyperfleet"

echo "running disk-to-mirror with archive-only input"
"$CONTAINER_TOOL" run --rm --network "$network" \
  -e HOME=/work/home -v "$disconnected:/work:z" -w /work \
  "$OC_MIRROR_IMAGE" "${auth_args[@]}" --v2 \
  --config imageset-config.yaml --from file://archive \
  --dest-tls-verify=false docker://$DESTINATION

destination_ref() {
  local source="$1"
  local repository="${source%@*}"
  repository="${repository#*/}"
  printf '%s/%s@%s' "$DESTINATION" "$repository" "${source##*@}"
}

for source in "$BUNDLE_IMAGE" "$OPERATOR_IMAGE" "$API_IMAGE"; do
  reference="$(destination_ref "$source")"
  echo "checking $reference"
  "$CONTAINER_TOOL" run --rm --network "$network" \
    -e HOME=/work/home -v "$disconnected:/work:z" -w /work \
    --entrypoint /usr/bin/skopeo "$OC_MIRROR_IMAGE" \
    inspect --tls-verify=false "docker://$reference" >/dev/null
done

echo "disconnected mirror transfer passed for bundle, operator, and API digests"
