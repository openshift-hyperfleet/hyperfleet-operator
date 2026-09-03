# Container image providing oc-mirror v2 and skopeo for disconnected mirroring tests.
# By default, downloads the official OpenShift release binary from mirror.openshift.com.

FROM registry.access.redhat.com/ubi9/ubi-minimal@sha256:7fbeae18dc9476399f565e68255f602a3374ea8614ba3d14843565131a13ff93

ARG TARGETARCH
ARG OCP_VERSION=4.18.18
ARG OC_MIRROR_X86_64_SHA256=b41059474ecfd1ba4ebae3aa7d052ea33f337097d9bea85a4363646c43d1822c
ARG OC_MIRROR_AARCH64_SHA256=7bdb10ea539d9d5e16338eb6df32f9998aca66d39f6156be9e0127a4d9779f64

RUN microdnf install -y \
    tar \
    gzip \
    ca-certificates \
    shadow-utils \
    skopeo \
    && microdnf clean all

RUN set -eux; \
    ARCH="${TARGETARCH:-$(uname -m)}"; \
    case "$ARCH" in \
        x86_64|amd64) ARCH_DIR="x86_64"; ARCHIVE_SHA256="$OC_MIRROR_X86_64_SHA256" ;; \
        aarch64|arm64) ARCH_DIR="aarch64"; ARCHIVE_SHA256="$OC_MIRROR_AARCH64_SHA256" ;; \
        *) echo "Unsupported architecture: $ARCH" >&2; exit 1 ;; \
    esac; \
    URL="https://mirror.openshift.com/pub/openshift-v4/${ARCH_DIR}/clients/ocp/${OCP_VERSION}/oc-mirror.tar.gz"; \
    echo "Downloading oc-mirror from ${URL}..."; \
    curl -fsSLo /tmp/oc-mirror.tar.gz "$URL"; \
    echo "${ARCHIVE_SHA256}  /tmp/oc-mirror.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/oc-mirror.tar.gz -C /usr/local/bin oc-mirror; \
    rm /tmp/oc-mirror.tar.gz; \
    chmod +x /usr/local/bin/oc-mirror; \
    /usr/local/bin/oc-mirror version || true

ENTRYPOINT ["/usr/local/bin/oc-mirror"]
