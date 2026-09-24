# Build stage
#
# Must be >= the `go` directive in go.mod (1.26.6), which the tinfoil-go
# dependency's own module graph pushed up (2026-09-24) — grpc 1.83 did the
# same thing once before, at 1.25. An older toolchain fails the build
# outright rather than degrading, so this pin and go.mod have to move
# together. Caught late here because `go mod tidy` silently
# auto-downloads a satisfying local toolchain (GOTOOLCHAIN=auto), so a
# local go build/vet/test all stayed green while this pin quietly fell
# behind — this image build is what actually enforces the two staying in
# sync, since it runs with no toolchain auto-download.
FROM golang:1.26-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o api-server ./cmd/api-server

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates

# Run as a dedicated non-root user (UID 1000 — must match the
# runAsUser in deploy/production/api-server.yaml). The binary must NOT
# live in /root/: that directory is 0700 and a non-root user cannot
# exec from it.
RUN adduser -D -u 1000 teepin

WORKDIR /app

# Copy binary from builder
COPY --from=builder /app/api-server .

RUN chown -R teepin:teepin /app
USER teepin

EXPOSE 8080

CMD ["./api-server"]
