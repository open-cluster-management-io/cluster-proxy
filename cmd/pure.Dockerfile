# Build the manager binary
# This dockerfile only used in middle stream build, without downloading and building APISERVER_NETWORK_PROXY_VERSION
FROM registry.ci.openshift.org/stolostron/builder:go1.19-linux AS builder

WORKDIR /workspace
COPY . .

# Build addons
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o agent cmd/addon-agent/main.go
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/addon-manager/main.go

# Use distroless as minimal base image to package the manager binary
FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
ENV USER_UID=10001

WORKDIR /
COPY --from=builder /workspace/agent /workspace/manager ./

RUN microdnf update && \
    microdnf clean all

USER ${USER_UID}
