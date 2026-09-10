#!/usr/bin/env bash
set -euo pipefail

CSV_FILE="${CSV_FILE:-/manifests/hyperfleet-operator.clusterserviceversion.yaml}"
YQ="${YQ:-yq}"

require_digest_pullspec() {
  local variable_name="$1"
  local pullspec="${!variable_name:-}"
  if [[ ! "${pullspec}" =~ ^[^[:space:]]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "error: ${variable_name} must be a non-empty sha256 digest pullspec, got '${pullspec}'" >&2
    exit 1
  fi
}

require_digest_pullspec HYPERFLEET_OPERATOR_IMAGE_PULLSPEC
require_digest_pullspec HYPERFLEET_API_IMAGE_PULLSPEC
[[ -f "${CSV_FILE}" ]] || { echo "error: CSV not found: ${CSV_FILE}" >&2; exit 1; }

# Fail before patching if the runtime override disappeared from the template.
# yq assignments to an empty selection can otherwise silently do nothing.
"${YQ}" eval -p yaml -o yaml -e '
  [.spec.install.spec.deployments[].spec.template.spec.containers[]
   | select(.name == "manager")] | length == 1
' "${CSV_FILE}" >/dev/null
if ! "${YQ}" eval -p yaml -o yaml -e '
  [.spec.install.spec.deployments[].spec.template.spec.containers[]
   | select(.name == "manager") | .env[]
   | select(.name == "RELATED_IMAGE_HYPERFLEET_API")] | length == 1
' "${CSV_FILE}" >/dev/null; then
  echo "error: CSV is missing exactly one RELATED_IMAGE_HYPERFLEET_API runtime override" >&2
  exit 1
fi

# Update image references in the CSV file using yq
"${YQ}" eval -p yaml -o yaml '
  # Update operator deployment image
  (.spec.install.spec.deployments[].spec.template.spec.containers[] | select(.name == "manager") | .image) = strenv(HYPERFLEET_OPERATOR_IMAGE_PULLSPEC) |

  # Update RELATED_IMAGE_HYPERFLEET_API env var
  (.spec.install.spec.deployments[].spec.template.spec.containers[] | select(.name == "manager") | .env[] | select(.name == "RELATED_IMAGE_HYPERFLEET_API") | .value) = strenv(HYPERFLEET_API_IMAGE_PULLSPEC) |

  # Update containerImage annotation
  .metadata.annotations.containerImage = strenv(HYPERFLEET_OPERATOR_IMAGE_PULLSPEC) |

  # Update relatedImages
  .spec.relatedImages = [
    {"name": "hyperfleet-operator", "image": strenv(HYPERFLEET_OPERATOR_IMAGE_PULLSPEC)},
    {"name": "hyperfleet-api", "image": strenv(HYPERFLEET_API_IMAGE_PULLSPEC)}
  ]
' -i "${CSV_FILE}"

cat "${CSV_FILE}"
