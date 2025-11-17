# Multi-stage build for e2e test binary
FROM golang:1.24-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build e2e test binary with ginkgo support
RUN go test -c -o /e2e ./test/e2e/

# Final stage - minimal image with just the binary
FROM alpine:3.22

# Install CA certificates for HTTPS calls
RUN apk --no-cache add ca-certificates

WORKDIR /

# Copy the e2e binary from builder
COPY --from=builder /e2e /e2e

# Make binary executable
RUN chmod +x /e2e

# Default entrypoint - can be overridden with additional args
# Supports Ginkgo v2 flags like --label-filter
ENTRYPOINT ["/e2e"]
