#!/usr/bin/env bash
set -euo pipefail

CSV_FILE="${CSV_FILE:-bundle/manifests/hyperfleet-operator.clusterserviceversion.yaml}"
image="${HYPERFLEET_OPERATOR_IMAGE_PULLSPEC:-}"
allow_tag=false

case "${1:-}" in
  "") ;;
  --allow-tag)
    allow_tag=true
    ;;
  *)
    echo "usage: $0 [--allow-tag]" >&2
    exit 1
    ;;
esac

if [[ "$image" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
  :
elif [[ "$allow_tag" == true && "$image" =~ ^[^[:space:]@]+:[^[:space:]@/]+$ ]]; then
  :
else
  if [[ "$allow_tag" == true ]]; then
    echo "error: HYPERFLEET_OPERATOR_IMAGE_PULLSPEC must be a sha256 digest pullspec or tagged pullspec" >&2
  else
    echo "error: HYPERFLEET_OPERATOR_IMAGE_PULLSPEC must be a sha256 digest pullspec" >&2
  fi
  exit 1
fi
[[ -f "$CSV_FILE" ]] || { echo "error: CSV not found: $CSV_FILE" >&2; exit 1; }

# operator-sdk derives operand relatedImages from RELATED_IMAGE_* variables but
# does not include the manager image itself. Insert that one generated entry
# without reserializing the whole generated CSV.
if grep -q '^[[:space:]]*name: hyperfleet-operator$' "$CSV_FILE"; then
  echo "error: CSV already contains a hyperfleet-operator relatedImages entry" >&2
  exit 1
fi
if [[ "$(grep -c '^  relatedImages:$' "$CSV_FILE")" -ne 1 ]]; then
  echo "error: expected exactly one spec.relatedImages block in $CSV_FILE" >&2
  exit 1
fi

tmp="$(mktemp "${CSV_FILE}.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
awk -v image="$image" '
  /^  relatedImages:$/ {
    print
    print "  - image: " image
    print "    name: hyperfleet-operator"
    next
  }
  { print }
' "$CSV_FILE" >"$tmp"
mv "$tmp" "$CSV_FILE"
trap - EXIT
