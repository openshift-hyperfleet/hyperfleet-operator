#!/usr/bin/env bash
set -euo pipefail

# Check the same CSV transformation used by the bundle build without
# modifying generated manifests or requiring a container runtime in PR CI.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_root="${1:-$root}"
YQ="${YQ:-yq}"
export YQ
command -v "$YQ" >/dev/null 2>&1 || { echo "required command: $YQ" >&2; exit 1; }

# Read defaults as data, never source/eval a Dockerfile. Duplicate or missing
# declarations fail the digest checks in update_bundle.sh.
for variable in HYPERFLEET_OPERATOR_IMAGE_PULLSPEC HYPERFLEET_API_IMAGE_PULLSPEC; do
  value="$(sed -n "s/^ARG ${variable}=\"\([^\"]*\)\"$/\1/p" "$source_root/bundle.konflux.Dockerfile")"
  export "$variable=$value"
done

workspace="$(mktemp -d "${TMPDIR:-/tmp}/hyperfleet-bundle-check.XXXXXX")"
trap 'rm -rf "$workspace"' EXIT
cp "$source_root/bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml" "$workspace/bundle.yaml"
export CSV_FILE="$workspace/bundle.yaml"
bash "$source_root/hack/bundle/update_bundle.sh" >/dev/null
cd "$root"
go run ./hack/verify-related-images -csv "$CSV_FILE"
