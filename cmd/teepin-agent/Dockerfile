# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# The TEEPIN cluster agent: runs on the GPU node, dials out to the
# control plane. Exposes no ports.

# Must be >= the `go` directive in go.mod.
FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Version is stamped into the binary and reported at registration, so an
# operator can tell which agents are running old code. Passed by
# build-agent.sh; "dev" when built by hand.
ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${VERSION}" \
    -o teepin-agent ./cmd/teepin-agent

# Runtime stage.
#
# The agent talks to two TLS endpoints (the control plane and the
# Kubernetes API), so it needs CA certificates and nothing else.
FROM alpine:latest

RUN apk --no-cache add ca-certificates

# UID 65534 (nobody) matches runAsUser in deploy/agent/agent.yaml. The
# binary lives in /usr/local/bin rather than /root, which is 0700 and
# would leave a non-root user unable to exec it.
COPY --from=builder /app/teepin-agent /usr/local/bin/teepin-agent

USER 65534:65534

ENTRYPOINT ["/usr/local/bin/teepin-agent"]
