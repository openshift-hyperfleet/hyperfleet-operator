FROM registry.access.redhat.com/ubi9/go-toolset:9.8-1789040808 AS validator

WORKDIR /workdir
COPY validators/ ./validators/
COPY go.mod go.mod
COPY go.sum go.sum

RUN go build -o ./bin/related-images-validator ./validators/related-images/

FROM quay.io/konflux-ci/operator-sdk-builder:latest@sha256:bd34ca58b2d08e8ee3b9cdf46b32f69173084ca09c1d3aba47285e2c35b4d1fc AS builder

WORKDIR /workdir
COPY config/ ./config/

COPY --from=validator /workdir/bin/related-images-validator ./related-images-validator
# Specify the kustomize variant, either bases/kustomization.yaml or prod/kustomization.yaml
# prod/kustomization.yaml gets image update references from konflux.
ARG KUSTOMIZE_VARIANT=config/manifests/dev
# ARG KUSTOMIZE_VARIANT=config/manifests/prod for konflux builds
RUN kustomize build /workdir/${KUSTOMIZE_VARIANT} > /workdir/manifests.yaml

ARG CHANNELS=stable
ARG DEFAULT_CHANNEL=stable
ARG BUNDLE_VERSION=0.0.1

RUN mkdir -p /workdir/bundle
RUN cat manifests.yaml | operator-sdk generate bundle -q --version ${BUNDLE_VERSION} \
      --channels=${CHANNELS} --default-channel=${DEFAULT_CHANNEL} \
      --package=hyperfleet-operator && \
    operator-sdk bundle validate ./bundle --select-optional name=operatorhubv2 && \
    if [ "${VALIDATE_RELATED_IMAGES}" = "true" ]; then \
      operator-sdk bundle validate ./bundle --alpha-select-external ./related-images-validator; \
    fi

FROM scratch

ARG CHANNELS=stable
ARG DEFAULT_CHANNEL=stable

# Core bundle labels.
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=hyperfleet-operator
LABEL operators.operatorframework.io.bundle.channels.v1=${CHANNELS}
LABEL operators.operatorframework.io.bundle.channel.default.v1=${DEFAULT_CHANNEL}
LABEL operators.operatorframework.io.metrics.builder=operator-sdk-v1.42.3
LABEL operators.operatorframework.io.metrics.mediatype.v1=metrics+v1
LABEL operators.operatorframework.io.metrics.project_layout=go.kubebuilder.io/v4

# Labels for testing.
LABEL operators.operatorframework.io.test.mediatype.v1=scorecard+v1
LABEL operators.operatorframework.io.test.config.v1=tests/scorecard/

# Copy patched manifests from builder, metadata and tests from source.
COPY --from=builder /workdir/bundle/manifests /manifests/
COPY --from=builder /workdir/bundle/metadata /metadata/
COPY --from=builder /workdir/bundle/tests/scorecard /tests/scorecard/

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