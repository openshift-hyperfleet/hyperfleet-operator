# OPM container image version. When invoked via the Makefile, this default is overridden by OPM_CONTAINER_VERSION; bumping only the line below will be ignored by make builds.
ARG OPM_VERSION=v1.69.0

# Named stage for OPM so COPY --from can reference it (variable expansion is not supported in --from).
FROM quay.io/operator-framework/opm:${OPM_VERSION} AS opm

# Use alpine for the build stage with shell support.
FROM alpine:latest AS builder

# Install OPM from the named stage.
COPY --from=opm /bin/opm /bin/opm

# Create containers policy configuration. Use standard development policy
# that matches Fedora's default configuration.
RUN mkdir -p /etc/containers && \
    echo '{"default":[{"type":"insecureAcceptAnything"}],"transports":{"docker-daemon":{"":[{"type":"insecureAcceptAnything"}]}}}' > /etc/containers/policy.json

WORKDIR /workspace

# COPY template file set as a build-arg
# Supports konflux + dev builds
ARG TEMPLATEFILE
COPY catalog/base-template.yaml ./
COPY catalog/${TEMPLATEFILE} ./

RUN cat base-template.yaml ${TEMPLATEFILE} > ./template.yaml

# Generate catalog for single template. Use symlink to make
# mount accessible to OPM. This allows OPM to read credentials without
# copying them to the filesystem.
RUN --mount=type=secret,id=dockerconfig,target=/run/secrets/auth.json \
    mkdir -p /root/.docker && \
    ln -s /run/secrets/auth.json /root/.docker/config.json && \
    /bin/opm alpha render-template basic \
        --migrate-level=bundle-object-to-csv-metadata \
        -o yaml ./template.yaml > catalog.yaml && \
    rm -f /root/.docker/config.json

# Serving stage
FROM opm

COPY --from=builder /workspace/catalog.yaml /configs/catalog.yaml

RUN ["/bin/opm", "serve", "/configs", "--cache-dir=/tmp/cache", "--cache-only"]

ENTRYPOINT ["/bin/opm"]
CMD ["serve", "/configs", "--cache-dir=/tmp/cache"]

LABEL operators.operatorframework.io.index.configs.v1=/configs
