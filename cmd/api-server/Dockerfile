# Build stage
FROM golang:1.21-alpine AS builder

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
