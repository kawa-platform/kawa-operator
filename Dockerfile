# Build the manager binary on the build host's platform and cross-compile for the target one, so
# multi-arch builds need no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# TARGETOS/TARGETARCH are set by BuildKit for the platform being built.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -ldflags "-s -w -X main.version=${VERSION}" -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
LABEL org.opencontainers.image.title="kawa-operator" \
      org.opencontainers.image.description="Kubernetes operator for the kawa Kafka gateway" \
      org.opencontainers.image.source="https://github.com/kawa-platform/kawa-operator" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
