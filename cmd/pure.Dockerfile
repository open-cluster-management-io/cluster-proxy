# Build the manager binary
# This dockerfile only used in middle stream build, without downloading and building APISERVER_NETWORK_PROXY_VERSION
FROM registry.ci.openshift.org/stolostron/builder:go1.17-linux AS builder

WORKDIR /workspace

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
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o manager cmd/addon-manager/main.go

# Use distroless as minimal base image to package the manager binary
FROM registry.access.redhat.com/ubi8/ubi-minimal:latest
ENV USER_UID=10001

WORKDIR /
COPY --from=builder /workspace/agent /workspace/manager ./
USER ${USER_UID}
