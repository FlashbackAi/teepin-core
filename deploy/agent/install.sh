#!/usr/bin/env bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# TEEPIN home-node installer (Linux core).
#
# Turns a Linux machine into a TEEPIN home compute node: installs k3s (unless
# present), drops the agent binary, enrolls with a one-time token, and runs
# the agent as a systemd service that survives reboots.
#
# This is the LINUX CORE. On Windows and macOS it is invoked INSIDE a Linux
# environment (WSL2 / a Lima VM) by the matching bootstrap script — the agent
# always runs inside Linux.
#
# Usage:
#   sudo bash install.sh --token <tne_...> --control-plane <https-api-url> \
#        [--grpc <host:port>] [--binary <path>] [--node-name <name>]
#
# The class is NOT a flag — it is fixed on the token by the operator. There is
# nothing here that lets a node choose to be a datacenter node.

set -euo pipefail

# --- args ----------------------------------------------------------------
TOKEN=""
CONTROL_PLANE=""       # https URL for enrollment (HTTP API)
GRPC_ADDR=""           # host:port for the agent's gRPC channel
BINARY=""              # path to a prebuilt teepin-agent; built from source if empty
NODE_NAME=""

while [ $# -gt 0 ]; do
    case "$1" in
        --token)         TOKEN="$2"; shift 2 ;;
        --control-plane) CONTROL_PLANE="$2"; shift 2 ;;
        --grpc)          GRPC_ADDR="$2"; shift 2 ;;
        --binary)        BINARY="$2"; shift 2 ;;
        --node-name)     NODE_NAME="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

info() { echo "[install] $*"; }
fail() { echo "[install] ERROR: $*" >&2; exit 1; }

# --- preconditions -------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || fail "this installer runs on Linux only. On Windows use bootstrap-windows.ps1 (WSL2); on macOS use bootstrap-macos.sh (Lima VM)."
[ "$(id -u)" = "0" ] || fail "run as root (sudo)."
[ -n "$TOKEN" ] || fail "--token is required (mint one in the control centre: Nodes -> Generate enrollment token)."
[ -n "$CONTROL_PLANE" ] || fail "--control-plane is required (e.g. https://api.teepin.com)."

# Derive the gRPC address from the control-plane host if not given. The HTTP
# API and the gRPC channel are the same host in production behind the ALB.
if [ -z "$GRPC_ADDR" ]; then
    host="${CONTROL_PLANE#*://}"; host="${host%%/*}"; host="${host%%:*}"
    GRPC_ADDR="${host}:443"
fi

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64)  GOARCH=amd64 ;;
    aarch64|arm64) GOARCH=arm64 ;;
    *) fail "unsupported architecture: $ARCH" ;;
esac
info "architecture: $ARCH ($GOARCH)"

# --- 1. k3s --------------------------------------------------------------
# k3s gives the node a real Kubernetes the agent runs pods on. Idempotent:
# the k3s installer no-ops if already installed.
if command -v k3s >/dev/null 2>&1; then
    info "k3s already installed"
else
    info "installing k3s..."
    curl -sfL https://get.k3s.io | sh - || fail "k3s install failed"
fi

# Wait for k3s to be ready so the agent finds a live cluster on first start.
info "waiting for k3s to be ready..."
for _ in $(seq 1 60); do
    if k3s kubectl get nodes >/dev/null 2>&1; then break; fi
    sleep 2
done
k3s kubectl get nodes >/dev/null 2>&1 || fail "k3s did not become ready in time"

# --- 2. agent binary -----------------------------------------------------
INSTALL_DIR=/usr/local/bin
AGENT_BIN="$INSTALL_DIR/teepin-agent"

if [ -n "$BINARY" ]; then
    info "installing agent from $BINARY"
    install -m 0755 "$BINARY" "$AGENT_BIN"
elif [ -f "$(dirname "$0")/teepin-agent" ]; then
    info "installing agent bundled next to this script"
    install -m 0755 "$(dirname "$0")/teepin-agent" "$AGENT_BIN"
else
    # Build from source when this script runs from a git checkout.
    command -v go >/dev/null 2>&1 || fail "no --binary given and 'go' is not installed to build one. Install Go, or pass --binary."
    info "building agent from source..."
    repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
    ( cd "$repo_root" && GOARCH="$GOARCH" go build -o "$AGENT_BIN" ./cmd/teepin-agent ) \
        || fail "agent build failed"
fi

# --- 3. enroll -----------------------------------------------------------
# The agent stores its credential under the teepin service user's home. Use a
# dedicated config path so the systemd unit and this enroll step agree.
CONFIG_DIR=/etc/teepin
CONFIG_FILE="$CONFIG_DIR/agent.json"
mkdir -p "$CONFIG_DIR"

enroll_args=(--token "$TOKEN" --control-plane "$CONTROL_PLANE")
[ -n "$NODE_NAME" ] && enroll_args+=(--node-name "$NODE_NAME")

info "enrolling with the control plane..."
TEEPIN_AGENT_CONFIG="$CONFIG_FILE" "$AGENT_BIN" enroll "${enroll_args[@]}" \
    || fail "enrollment failed (token invalid, expired, or already used?)"

# --- 4. systemd service --------------------------------------------------
# k3s writes its kubeconfig here; KUBECONFIG points the agent at it so it runs
# workloads on the local cluster.
cat > /etc/systemd/system/teepin-agent.service <<EOF
[Unit]
Description=TEEPIN home compute agent
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Environment=TEEPIN_AGENT_CONFIG=$CONFIG_FILE
Environment=TEEPIN_CONTROL_PLANE=$GRPC_ADDR
Environment=KUBECONFIG=/etc/rancher/k3s/k3s.yaml
ExecStart=$AGENT_BIN run
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

info "starting the agent service..."
systemctl daemon-reload
systemctl enable --now teepin-agent.service

info "done. The node should appear in the control centre (Nodes) as online within a minute."
info "check status with:  systemctl status teepin-agent"
info "follow logs with:   journalctl -u teepin-agent -f"
