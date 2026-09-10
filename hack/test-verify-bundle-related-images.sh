#!/usr/bin/env bash
set -euo pipefail

# Negative checks run through the real bundle-image gate with temporary packaging
# inputs, rather than only testing hand-written verifier CSV fixtures.
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
YQ="${YQ:-yq}"
export YQ
workspace="$(mktemp -d "${TMPDIR:-/tmp}/hyperfleet-bundle-test.XXXXXX")"
trap 'rm -rf "$workspace"' EXIT
mkdir -p "$workspace/bundle/manifests" "$workspace/hack/bundle"
csv="$workspace/bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml"

reset_fixture() {
  cp "$root/bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml" "$csv"
  cp "$root/bundle.konflux.Dockerfile" "$workspace/bundle.konflux.Dockerfile"
  cp "$root/hack/bundle/update_bundle.sh" "$workspace/hack/bundle/update_bundle.sh"
}

bundle_image_args() {
  sed -n 's/^ARG \(HYPERFLEET_[A-Z0-9_]*_IMAGE_PULLSPEC\)="[^"]*"$/\1/p' "$1"
}

load_bundle_image_pullspecs() {
  local image_arg value image_arg_count=0

  while IFS= read -r image_arg; do
    value="$(sed -n "s/^ARG ${image_arg}=\"\\([^\"]*\\)\"$/\\1/p" "$workspace/bundle.konflux.Dockerfile")"
    [[ -n "$value" ]] || {
      echo "missing default for bundle image argument: $image_arg" >&2
      exit 1
    }
    export "$image_arg=$value"
    ((image_arg_count += 1))
  done < <(bundle_image_args "$workspace/bundle.konflux.Dockerfile")
  ((image_arg_count > 0)) || {
    echo "bundle Dockerfile has no image pullspec arguments" >&2
    exit 1
  }
}

expect_failure() {
  local description="$1"
  local expected_message="$2"

  if bash "$root/hack/verify-bundle-related-images.sh" "$workspace" >"$workspace/result.log" 2>&1; then
    echo "bundle-image gate unexpectedly accepted: $description" >&2
    exit 1
  fi
  if ! grep -Fq -- "$expected_message" "$workspace/result.log"; then
    echo "bundle-image gate returned the wrong diagnostic for: $description" >&2
    cat "$workspace/result.log" >&2
    exit 1
  fi
  echo "bundle-image negative check passed: $description"
}

reset_fixture
"$YQ" eval -i '
  del(.spec.install.spec.deployments[].spec.template.spec.containers[].env[] |
      select(.name == "RELATED_IMAGE_HYPERFLEET_API")) |
  del(.spec.relatedImages[] | select(.name == "hyperfleet-api"))
' "$csv"
expect_failure \
  "API missing from both override and relatedImages" \
  "CSV is missing exactly one RELATED_IMAGE_HYPERFLEET_API runtime override"

reset_fixture
load_bundle_image_pullspecs
CSV_FILE="$csv" bash "$workspace/hack/bundle/update_bundle.sh" >/dev/null
"$YQ" eval -r '.spec.relatedImages[]?.name | select(. != null)' "$csv" \
  >"$workspace/related-image-names"

related_image_count=0
while IFS= read -r related_image; do
  ((related_image_count += 1))
  reset_fixture
  # Simulate a regression in bundle packaging that drops each declared image
  # after update_bundle.sh has populated the final CSV.
  export RELATED_IMAGE_NAME="$related_image"
  printf '\n"$YQ" eval -i '\''del(.spec.relatedImages[] | select(.name == strenv(RELATED_IMAGE_NAME)))'\'' "$CSV_FILE"\n' \
    >>"$workspace/hack/bundle/update_bundle.sh"
  expect_failure \
    "$related_image missing from the transformed bundle CSV" \
    "CSV spec.relatedImages is missing CSV image sources entry \"$related_image\""
  unset RELATED_IMAGE_NAME
done <"$workspace/related-image-names"
((related_image_count > 0)) || {
  echo "bundle CSV has no relatedImages to test" >&2
  exit 1
}

image_arg_count=0
while IFS= read -r image_arg; do
  [[ "$image_arg" =~ ^HYPERFLEET_[A-Z0-9_]+_IMAGE_PULLSPEC$ ]] || {
    echo "unexpected bundle image argument: $image_arg" >&2
    exit 1
  }
  ((image_arg_count += 1))
  reset_fixture
  sed -E "s|^ARG ${image_arg}=\"[^\"]*\"$|ARG ${image_arg}=\"example.com/invalid:latest\"|" \
    "$root/bundle.konflux.Dockerfile" >"$workspace/bundle.konflux.Dockerfile"
  expect_failure \
    "mutable $image_arg" \
    "$image_arg must be a non-empty sha256 digest pullspec"
done < <(bundle_image_args "$root/bundle.konflux.Dockerfile")
((image_arg_count > 0)) || {
  echo "bundle Dockerfile has no image pullspec arguments to test" >&2
  exit 1
}
