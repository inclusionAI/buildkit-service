# The buildkit-service image: layers the HTTP build API (buildctl-daemon,
# buildctl-batch), debug tools (grpcurl, curl) and CNI plugins on top of the
# upstream Nydus-enabled BuildKit image
# (https://github.com/nydusaccelerator/buildkit). The result serves both chart
# roles: the buildkitd worker `image` (CNI-capable, plugins in /usr/lib/cni
# matching the chart's default cniBinaryPath) and `buildctlDaemon.image`.
#
# Build from the repository root:
#   docker buildx build --platform linux/amd64 \
#     -f chart/images/buildkit/Dockerfile \
#     -t ghcr.io/inclusionai/buildkit-service/buildctl-daemon:dev --push .
#
# Override BASE_IMAGE to layer on a different BuildKit image:
#   docker buildx build --build-arg BASE_IMAGE=moby/buildkit:latest ...
ARG BASE_IMAGE=ghcr.io/nydusaccelerator/buildkit:v0.31.1-nydus.1

ARG GO_IMAGE=golang:1.23
ARG ALPINE_IMAGE=alpine:3.22

FROM ${GO_IMAGE} AS builder
WORKDIR /src
# Batch result storage uses LMDB. Link it statically so the runtime base does
# not need a matching libc or a separately installed liblmdb.
RUN apt-get update && apt-get install -y --no-install-recommends liblmdb-dev \
    && rm -rf /var/lib/apt/lists/*
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY pkg/ pkg/
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /out/buildctl-daemon ./cmd/buildctl-daemon && \
    CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -tags netgo,osusergo -ldflags '-linkmode external -extldflags "-static"' \
    -o /out/buildctl-batch ./cmd/buildctl-batch && \
    GOBIN=/out CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go install github.com/fullstorydev/grpcurl/cmd/grpcurl@v1.9.3

FROM ${ALPINE_IMAGE} AS cni-plugins
ARG CNI_VERSION=v1.1.1
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache curl
RUN mkdir -p /out \
    && curl -fsSL "https://github.com/containernetworking/plugins/releases/download/${CNI_VERSION}/cni-plugins-${TARGETOS:-linux}-${TARGETARCH:-amd64}-${CNI_VERSION}.tgz" \
       | tar -xz -C /out

FROM ${BASE_IMAGE}
# iptables/ip6tables are required by the CNI bridge and portmap plugins; without
# them buildkitd aborts at startup with
# `plugin type="bridge" failed (add): failed to locate iptables`.
RUN if command -v apk >/dev/null 2>&1; then \
        apk add --no-cache curl iptables ip6tables; \
    elif command -v apt-get >/dev/null 2>&1; then \
        apt-get update && \
        apt-get install -y --no-install-recommends ca-certificates curl iptables && \
        rm -rf /var/lib/apt/lists/*; \
    else \
        echo "BASE_IMAGE must provide apk or apt-get so curl can be installed" >&2; \
        exit 1; \
    fi
COPY --from=cni-plugins /out/ /usr/lib/cni/
COPY --from=builder /out/buildctl-daemon /usr/bin/buildctl-daemon
COPY --from=builder /out/buildctl-batch /usr/bin/buildctl-batch
COPY --from=builder /out/grpcurl /usr/bin/grpcurl
