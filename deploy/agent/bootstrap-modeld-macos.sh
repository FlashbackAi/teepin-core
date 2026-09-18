#!/usr/bin/env bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# TEEPIN host agent for macOS (Apple Silicon) -- the ONE installer and ONE
# enrollment for a Mac.
#
# The agent runs on the macOS host and drives two runtimes under a single node
# identity:
#   * native host processes -- MLX model servers, which need Metal and so
#     cannot live in a VM;
#   * (optional, --with-vm) a Lima Linux VM running k3s -- customer containers.
#
# NEW Mac:
#   bash bootstrap-modeld-macos.sh --binary ./teepin-agent-darwin-arm64 \
#       --token <tne_...> --control-plane https://api.teepin.com \
#       --grpc api.teepin.com:9090 [--node-name mac-mini] [--with-vm]
#
# Mac ALREADY ENROLLED via bootstrap-macos.sh (agent inside the VM): no token,
# no second enrollment. This adopts the VM agent's credential, stops the VM
# agent (two agents must never share a credential), and keeps the VM's k3s for
# containers:
#   bash bootstrap-modeld-macos.sh --binary ./teepin-agent-darwin-arm64 --with-vm
#
# Re-run with just --binary to update the agent binary.
#
# Options:
#   --binary PATH        darwin/arm64 teepin-agent (required)
#   --token / --control-plane / --grpc / --node-name    first-install enrollment
#   --with-vm [NAME]     also run container workloads via Lima k3s (default VM: teepin)
#   --vm-memory GiB      memory for a NEWLY created VM (default 4)
#   --vm-cpus N          CPUs for a NEWLY created VM (default 2)

set -euo pipefail

BINARY=""
TOKEN=""
CONTROL_PLANE=""
GRPC_ADDR=""
NODE_NAME=""
WITH_VM=false
VM_NAME="teepin"
VM_MEM_GIB=4
VM_CPUS=2

while [ $# -gt 0 ]; do
    case "$1" in
        --binary)        BINARY="$2"; shift 2 ;;
        --token)         TOKEN="$2"; shift 2 ;;
        --control-plane) CONTROL_PLANE="$2"; shift 2 ;;
        --grpc)          GRPC_ADDR="$2"; shift 2 ;;
        --node-name)     NODE_NAME="$2"; shift 2 ;;
        --with-vm)       WITH_VM=true
                         if [ $# -gt 1 ] && [ "${2#--}" = "$2" ]; then VM_NAME="$2"; shift; fi
                         shift ;;
        --vm-memory)     VM_MEM_GIB="$2"; shift 2 ;;
        --vm-cpus)       VM_CPUS="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

info() { echo "[modeld] $*"; }
fail() { echo "[modeld] ERROR: $*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || fail "macOS only."
[ "$(uname -m)" = "arm64" ] || fail "MLX needs Apple Silicon."
[ "$(id -u)" -ne 0 ] || fail "run as your normal user, not root."
[ -n "$BINARY" ] && [ -f "$BINARY" ] || fail "--binary <path to the darwin/arm64 teepin-agent> is required."

STATE_DIR="$HOME/.teepin/modeld"
BIN_DIR="$HOME/.teepin/bin"
CONFIG="$STATE_DIR/agent.json"
KUBECONFIG_FILE="$STATE_DIR/kubeconfig"
PLIST="$HOME/Library/LaunchAgents/com.teepin.modeld.plist"
LABEL="com.teepin.modeld"
mkdir -p "$STATE_DIR" "$BIN_DIR"

# --- 1. MLX server -------------------------------------------------------
if ! command -v mlx_lm.server >/dev/null 2>&1 && [ ! -x "$HOME/.local/bin/mlx_lm.server" ]; then
    if ! command -v uv >/dev/null 2>&1; then
        command -v brew >/dev/null 2>&1 || fail "Homebrew is required to install uv. See https://brew.sh"
        info "installing uv..."
        brew install uv
    fi
    info "installing mlx-lm..."
    uv tool install mlx-lm
fi
MLX_BIN="$(command -v mlx_lm.server || echo "$HOME/.local/bin/mlx_lm.server")"
[ -x "$MLX_BIN" ] || fail "mlx_lm.server not found after install."
info "mlx server: $MLX_BIN"

# --- 2. Agent binary -----------------------------------------------------
install -m 0755 "$BINARY" "$BIN_DIR/teepin-agent"
xattr -d com.apple.quarantine "$BIN_DIR/teepin-agent" 2>/dev/null || true
info "installed $BIN_DIR/teepin-agent"

# --- 3. Container runtime (optional): Lima VM with k3s -------------------
RUNTIME="native"
ADOPTED_FROM_VM=false
if $WITH_VM; then
    RUNTIME="hybrid"
    if ! command -v limactl >/dev/null 2>&1; then
        command -v brew >/dev/null 2>&1 || fail "Homebrew is required to install Lima. See https://brew.sh"
        info "installing Lima..."
        brew install lima
    fi

    if limactl list --quiet 2>/dev/null | grep -qx "$VM_NAME"; then
        info "Lima VM '$VM_NAME' exists; ensuring it is running..."
        limactl start "$VM_NAME" >/dev/null 2>&1 || true
    else
        info "creating Lima VM '$VM_NAME' (${VM_CPUS} CPUs / ${VM_MEM_GIB}GiB)..."
        limactl start --name "$VM_NAME" --cpus "$VM_CPUS" --memory "${VM_MEM_GIB}GiB" template://ubuntu
    fi
    limactl start-at-login "$VM_NAME" >/dev/null 2>&1 || true

    vmsh() { limactl shell "$VM_NAME" -- "$@"; }

    # An agent inside the VM means this Mac was enrolled the old way. Adopt
    # its credential and stop it: one enrollment, one agent per credential.
    if [ ! -f "$CONFIG" ] && vmsh test -f /etc/teepin/agent.json 2>/dev/null; then
        info "found an existing enrollment inside the VM; adopting it (no new token needed)."
        vmsh sudo cat /etc/teepin/agent.json > "$CONFIG.tmp"
        install -m 0600 "$CONFIG.tmp" "$CONFIG" && rm -f "$CONFIG.tmp"
        if [ -z "$GRPC_ADDR" ]; then
            GRPC_ADDR="$(vmsh sudo cat /etc/systemd/system/teepin-agent.service 2>/dev/null \
                | grep -o 'TEEPIN_CONTROL_PLANE=[^" ]*' | head -1 | cut -d= -f2- || true)"
        fi
        ADOPTED_FROM_VM=true
    fi
    if vmsh systemctl is-active --quiet teepin-agent 2>/dev/null; then
        info "stopping the in-VM agent (the host agent takes over its identity)..."
        vmsh sudo systemctl disable --now teepin-agent
    fi

    if ! vmsh command -v k3s >/dev/null 2>&1; then
        info "installing k3s in the VM..."
        vmsh bash -c 'curl -sfL https://get.k3s.io | sudo sh -'
    fi
    info "waiting for k3s..."
    for _ in $(seq 1 60); do
        vmsh sudo k3s kubectl get nodes >/dev/null 2>&1 && break
        sleep 2
    done
    vmsh sudo cat /etc/rancher/k3s/k3s.yaml > "$KUBECONFIG_FILE"
    chmod 600 "$KUBECONFIG_FILE"
    if ! curl -ks --max-time 5 https://127.0.0.1:6443/version >/dev/null 2>&1; then
        info "WARNING: the VM's k3s API is not reachable on 127.0.0.1:6443 from the host."
        info "         Container workloads will not run until Lima forwards that port."
    fi
fi

# --- 4. Enroll (first install only) --------------------------------------
export TEEPIN_AGENT_CONFIG="$CONFIG"
if [ -f "$CONFIG" ]; then
    $ADOPTED_FROM_VM || info "already enrolled ($CONFIG); updating binary only."
else
    [ -n "$TOKEN" ] || fail "--token is required for a first install."
    [ -n "$CONTROL_PLANE" ] || fail "--control-plane is required for a first install."
    [ -n "$NODE_NAME" ] || NODE_NAME="$(hostname -s)"
    info "enrolling as '$NODE_NAME'..."
    TEEPIN_RUNTIME="$RUNTIME" "$BIN_DIR/teepin-agent" enroll \
        --token "$TOKEN" --control-plane "$CONTROL_PLANE" --node-name "$NODE_NAME"
fi
if [ -z "$GRPC_ADDR" ]; then
    GRPC_ADDR="$(/usr/bin/plutil -extract EnvironmentVariables.TEEPIN_CONTROL_PLANE raw "$PLIST" 2>/dev/null || true)"
fi
[ -n "$GRPC_ADDR" ] || fail "--grpc <host:port> is required (could not be discovered)."

# --- 5. launchd job ------------------------------------------------------
KUBECONFIG_ENTRY=""
if [ "$RUNTIME" = "hybrid" ]; then
    KUBECONFIG_ENTRY="<key>KUBECONFIG</key><string>$KUBECONFIG_FILE</string>"
fi
cat > "$PLIST" <<PLISTEOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>$LABEL</string>
    <key>ProgramArguments</key>
    <array><string>$BIN_DIR/teepin-agent</string><string>run</string></array>
    <key>EnvironmentVariables</key>
    <dict>
        <key>TEEPIN_RUNTIME</key><string>$RUNTIME</string>
        <key>TEEPIN_CONTROL_PLANE</key><string>$GRPC_ADDR</string>
        <key>TEEPIN_AGENT_CONFIG</key><string>$CONFIG</string>
        <key>TEEPIN_MODELD_DIR</key><string>$STATE_DIR</string>
        $KUBECONFIG_ENTRY
        <key>PATH</key><string>$(dirname "$MLX_BIN"):/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>$STATE_DIR/agent.log</string>
    <key>StandardErrorPath</key><string>$STATE_DIR/agent.log</string>
</dict>
</plist>
PLISTEOF

launchctl bootout "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
info "running in '$RUNTIME' mode. Logs: tail -f $STATE_DIR/agent.log"
info "Next: Control Center -> Inference -> register a model (engine mlx) -> mount it on this node."
