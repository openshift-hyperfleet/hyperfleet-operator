FROM registry.k8s.io/kustomize/kustomize:v5.0.0 AS kustomize

COPY config/ /workdir/config/

# Override the base kustomization.yaml
ARG KUSTOMIZE_VARIANT=dev/kustomization.yaml
RUN cp /workdir/config/manager/${KUSTOMIZE_VARIANT} \
       /workdir/config/manager/kustomization.yaml && \
    kustomize build /workdir/config/manifests > /workdir/manifests.yaml

FROM quay.io/operator-framework/operator-sdk:v1.42.3 AS operator
COPY --from=kustomize /workdir/manifests.yaml /workdir/manifests.yaml
ARG CHANNELS=stable
ARG VERSION=0.0.1
WORKDIR /workdir
RUN cat manifests.yaml | operator-sdk generate bundle -q --version ${VERSION} \
      --channels=${CHANNELS} --default-channel=stable \
      --package=hyperfleet-operator && \
    operator-sdk bundle validate ./bundle

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
COPY --from=operator /workdir/bundle/manifests /manifests/
COPY --from=operator /workdir/bundle/metadata /metadata/
COPY --from=operator /workdir/bundle/tests/scorecard /tests/scorecard/

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
