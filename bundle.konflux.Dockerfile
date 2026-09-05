# Konflux bundle image build. Unlike the auto-generated bundle.Dockerfile (used
# for local dev with operator-sdk), this runs bundle-hack/update_bundle.sh to
# patch digest-pinned image references into the CSV at build time.
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS builder-runner
RUN microdnf install -y tar gzip && \
    curl -sL https://github.com/mikefarah/yq/releases/download/v4.44.1/yq_linux_amd64.tar.gz | tar xz && \
    mv yq_linux_amd64 /usr/bin/yq

FROM builder-runner AS builder
# Hack to set the operator container image in the deployment
# Konflux nudges update these variables with the latest digest-pinned pullspecs.
ARG HYPERFLEET_OPERATOR_IMAGE_PULLSPEC="quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator@sha256:e8892606efb2238f69fcddbb0549fd4d9b9f18c93afed65fe98b03ac0bfdea5f"
ENV HYPERFLEET_OPERATOR_IMAGE_PULLSPEC=${HYPERFLEET_OPERATOR_IMAGE_PULLSPEC}

ARG HYPERFLEET_API_IMAGE_PULLSPEC="quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api@sha256:8533d0d875480f31f5112e454659a095a5d2e993c139a9045a06be6b67b829ca"
ENV HYPERFLEET_API_IMAGE_PULLSPEC=${HYPERFLEET_API_IMAGE_PULLSPEC}

COPY bundle-hack .
COPY bundle/manifests /manifests/

RUN ./update_bundle.sh

FROM scratch

# Core bundle labels.
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=hyperfleet-operator
LABEL operators.operatorframework.io.bundle.channels.v1=stable
LABEL operators.operatorframework.io.bundle.channel.default.v1=stable
LABEL operators.operatorframework.io.metrics.builder=operator-sdk-v1.42.3
LABEL operators.operatorframework.io.metrics.mediatype.v1=metrics+v1
LABEL operators.operatorframework.io.metrics.project_layout=go.kubebuilder.io/v4

# Labels for testing.
LABEL operators.operatorframework.io.test.mediatype.v1=scorecard+v1
LABEL operators.operatorframework.io.test.config.v1=tests/scorecard/

# Copy patched manifests from builder, metadata and tests from source.
COPY --from=builder /manifests /manifests/
COPY bundle/metadata /metadata/
COPY bundle/tests/scorecard /tests/scorecard/


ARG APP_VERSION="0.0.0-dev"
LABEL name="hyperfleet-operator-bundle" \
      vendor="Red Hat, Inc." \
      version="${APP_VERSION}" \
      summary="OLM bundle for the HyperFleet Operator" \
      description="OLM bundle for the HyperFleet Operator, which installs and manages HyperFleet." \
      com.redhat.component="hyperfleet-operator-bundle-container" \
      io.k8s.description="OLM bundle for the HyperFleet Operator, which installs and manages HyperFleet." \
      distribution-scope="public" \
      release="1" \
      url="https://github.com/openshift-hyperfleet/hyperfleet-operator" \
      maintainer="Red Hat HyperFleet Team"
