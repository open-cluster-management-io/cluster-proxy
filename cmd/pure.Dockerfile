# Build the manager binary
# This dockerfile only used in middle stream build, without downloading and building APISERVER_NETWORK_PROXY_VERSION
FROM registry.ci.openshift.org/stolostron/builder:go1.23-linux AS builder

WORKDIR /workspace
COPY . .

# Build apiserver-network-proxy binaries from git submodule (third_party/apiserver-network-proxy)
# The submodule is at version v0.1.6-patch-03 from https://github.com/stolostron/apiserver-network-proxy
RUN cd third_party/apiserver-network-proxy && \
    CGO_ENABLED=1 go build -o proxy-agent cmd/agent/main.go && \
    CGO_ENABLED=1 go build -o proxy-server cmd/server/main.go

# Build addons
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o agent cmd/addon-agent/main.go
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/addon-manager/main.go
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o cluster-proxy cmd/cluster-proxy/main.go

# Use distroless as minimal base image to package the manager binary
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
ENV USER_UID=10001

WORKDIR /
COPY --from=builder /workspace/agent /workspace/manager /workspace/cluster-proxy ./

# Copy apiserver-network-proxy binaries
COPY --from=builder /workspace/third_party/apiserver-network-proxy/proxy-agent ./proxy-agent
COPY --from=builder /workspace/third_party/apiserver-network-proxy/proxy-server ./proxy-server

RUN microdnf update -y && \
    microdnf clean all

USER ${USER_UID}
