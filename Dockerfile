ARG BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest

FROM registry.access.redhat.com/ubi9/go-toolset:9.8-1792010711 AS builder

# No .git in the build container, so APP_VERSION/GIT_SHA (embedded via
# -ldflags -X in `make build`, see internal/version) arrive as build-args.
ARG APP_VERSION="0.0.0-dev"
ARG GIT_SHA="unknown"

# Create /build as root and hand it to the non-root build user.
USER root
WORKDIR /build
RUN chown 1001:0 /build
USER 1001

ENV GOBIN=/build/.gobin
RUN mkdir -p $GOBIN

COPY --chown=1001:0 go.mod go.sum ./
RUN --mount=type=cache,target=/opt/app-root/src/go/pkg/mod,uid=1001 \
    go mod download

COPY --chown=1001:0 . .

# Enable FIPS only in the image: select the validated module and include fips.go.
# no_openssl selects Go's native backend; GOTOOLCHAIN=local keeps Red Hat Go.
RUN --mount=type=cache,target=/opt/app-root/src/go/pkg/mod,uid=1001 \
    --mount=type=cache,target=/opt/app-root/src/.cache/go-build,uid=1001 \
    GOFIPS140=v1.0.0 \
    make build GOTOOLCHAIN=local GOFLAGS="-trimpath -tags=fips,no_openssl" APP_VERSION=${APP_VERSION} GIT_SHA=${GIT_SHA}

# Check the selected module, compiled default, and startup on a simulated FIPS node.
RUN go version -m /build/bin/manager | grep -Eq 'GOFIPS140=v1[.]0[.]0(-[[:xdigit:]]+)?$' || { echo "FIPS check FAILED: missing GOFIPS140=v1.0.0"; exit 1; }
RUN go version -m /build/bin/manager \
    | grep -E '^[[:space:]]*build[[:space:]]+DefaultGODEBUG=([^,]*,)*fips140=on(,|$)' \
    | grep -Ev 'DefaultGODEBUG=([^,]*,)*fips140ems=0(,|$)' \
    || { echo "FIPS check FAILED: require fips140=on without fips140ems=0"; exit 1; }
RUN GOLANG_FIPS=1 /build/bin/manager --help >/dev/null || { echo "FIPS check FAILED: binary does not start in FIPS mode"; exit 1; }

# Runtime stage
FROM ${BASE_IMAGE} AS final

WORKDIR /
COPY --from=builder /build/bin/manager .
COPY --from=builder /build/LICENSE /licenses/LICENSE
USER 65532:65532

ARG APP_VERSION="0.0.0-dev"

ENTRYPOINT ["/manager"]

LABEL name="hyperfleet-operator" \
      vendor="Red Hat, Inc." \
      version="${APP_VERSION}" \
      summary="HyperFleet Operator - A Kubernetes operator that packages and delivers HyperFleet." \
      description="A Kubernetes operator that packages and delivers HyperFleet." \
      com.redhat.component="hyperfleet-operator-container" \
      io.k8s.description="A Kubernetes operator that packages and delivers HyperFleet." \
      distribution-scope="public" \
      release="1" \
      url="https://github.com/openshift-hyperfleet/hyperfleet-operator" \
      maintainer="Red Hat HyperFleet Team"
