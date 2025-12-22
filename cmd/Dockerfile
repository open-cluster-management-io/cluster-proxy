# Build the manager binary
FROM golang:1.25 AS builder

WORKDIR /workspace

ARG APISERVER_NETWORK_PROXY_VERSION=0.30.2
ARG KUBECTL_VERSION=v1.30.2
ARG ADDON_AGENT_IMAGE_NAME

# Build apiserver-network-proxy binaries from git submodule (third_party/apiserver-network-proxy)
# The submodule is at version v0.1.6-patch-03 from https://github.com/stolostron/apiserver-network-proxy
RUN cd third_party/apiserver-network-proxy && \
    CGO_ENABLED=1 go build -o proxy-agent cmd/agent/main.go && \
    CGO_ENABLED=1 go build -o proxy-server cmd/server/main.go

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY pkg pkg/

# Build addons
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o agent cmd/addon-agent/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a \
        -ldflags="-X 'open-cluster-management.io/cluster-proxy/pkg/config.AgentImageName=${ADDON_AGENT_IMAGE_NAME}'" \
        -o manager cmd/addon-manager/main.go
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o cluster-proxy cmd/cluster-proxy/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM alpine:3.21

WORKDIR /
RUN apk add libc6-compat

COPY --from=builder /workspace/agent /workspace/manager ./
COPY --from=builder /workspace/cluster-proxy ./

# Copy apiserver-network-proxy binaries
COPY --from=builder /workspace/third_party/apiserver-network-proxy/proxy-agent ./proxy-agent
COPY --from=builder /workspace/third_party/apiserver-network-proxy/proxy-server ./proxy-server

USER 65532:65532
