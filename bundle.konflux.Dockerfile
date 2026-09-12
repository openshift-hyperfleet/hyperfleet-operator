# Konflux bundle image build. Unlike the auto-generated bundle.Dockerfile used
# for local development, this runs hack/bundle/update_bundle.sh to
# patch digest-pinned image references into the CSV at build time.
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest AS builder-runner
RUN microdnf install -y tar gzip && \
    curl -fsSLo /tmp/yq.tar.gz https://github.com/mikefarah/yq/releases/download/v4.44.1/yq_linux_amd64.tar.gz && \
    tar -xzf /tmp/yq.tar.gz && \
    mv yq_linux_amd64 /usr/bin/yq && \
    rm /tmp/yq.tar.gz

FROM builder-runner AS builder
# Hack to set the operator container image in the deployment
# Konflux nudges update these variables with the latest digest-pinned pullspecs.
ARG HYPERFLEET_OPERATOR_IMAGE_PULLSPEC="quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-operator@sha256:45de8db3123a8414263c6de33c09fc49fe47092e291e94cc945c8c5cbd9d6b4d"
ENV HYPERFLEET_OPERATOR_IMAGE_PULLSPEC=${HYPERFLEET_OPERATOR_IMAGE_PULLSPEC}

ARG HYPERFLEET_API_IMAGE_PULLSPEC="quay.io/redhat-services-prod/hyperfleet-tenant/hyperfleet/hyperfleet-api@sha256:99f8cdda580069de21ba0e13b5b171cf82b81b93dc88b12bcaa8294e72e84fc3"
ENV HYPERFLEET_API_IMAGE_PULLSPEC=${HYPERFLEET_API_IMAGE_PULLSPEC}

COPY hack/bundle .
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
