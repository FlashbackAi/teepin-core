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
# Re-run on an ALREADY-enrolled node (detected by /etc/teepin/agent.json and
# the systemd unit both existing) to UPDATE the agent binary in place: no
# token needed, k3s and enrollment are untouched. This exists because a plain
# `cp` over the running binary fails with "Text file busy" — the service must
# be stopped first — which is easy to get wrong by hand.
#
# This is the LINUX CORE. On Windows and macOS it is invoked INSIDE a Linux
# environment (WSL2 / a Lima VM) by the matching bootstrap script — the agent
# always runs inside Linux.
#
# Also (re)installs the ECR pull-secret refresh timer (refresh-ecr-pull-secret.sh,
# bundled next to this script) on EVERY run, install or update alike — a
# Kumbha deploy to this node pulls its built image from a private ECR
# repository, which needs a token refreshed well inside its 12-hour expiry.
# This is done here, unconditionally, rather than left as a separate step an
# operator has to remember to run once and then forget about: found live
# 2026-09-07, a node's pull secret was a single manual one-off from two
# weeks earlier with no recurring refresh ever set up, and image pulls
# started failing with a 403 the moment that token finally expired. The one
# piece that stays manual is populating the AWS credential itself
# (/etc/teepin/kumbha-ecr-puller.env) — this script prints exactly what to
# run if that file is still empty.
#
# Usage:
#   # First install:
#   sudo bash install.sh --token <tne_...> --control-plane <https-api-url> \
#        [--grpc <host:port>] [--binary <path>] [--node-name <name>] \
#        [--ecr-account-id <id>] [--ecr-region <region>]
#
#   # Update an existing node (after a `go build`, or with --binary):
#   sudo bash install.sh [--binary <path>]
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
# Only one AWS account/region has ever backed this platform's ECR registry,
# so these default to it — override only if a future environment differs.
ECR_ACCOUNT_ID="880254196251"
ECR_REGION="us-east-1"

while [ $# -gt 0 ]; do
    case "$1" in
        --token)         TOKEN="$2"; shift 2 ;;
        --control-plane) CONTROL_PLANE="$2"; shift 2 ;;
        --grpc)          GRPC_ADDR="$2"; shift 2 ;;
        --binary)        BINARY="$2"; shift 2 ;;
        --node-name)     NODE_NAME="$2"; shift 2 ;;
        --ecr-account-id) ECR_ACCOUNT_ID="$2"; shift 2 ;;
        --ecr-region)     ECR_REGION="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

info() { echo "[install] $*"; }
fail() { echo "[install] ERROR: $*" >&2; exit 1; }

# apply_pe_core_env (idempotently) writes TEEPIN_PCORES/TEEPIN_ECORES into
# the given systemd unit's [Service] Environment lines and reloads systemd
# so the change is picked up on the unit's next start. A no-op when
# TEEPIN_PCORES/TEEPIN_ECORES are not both set in THIS script's own
# environment — the bootstrap script only exports them when
# teepin-hostprobe actually reported a usable split (see
# bootstrap-windows.ps1/bootstrap-macos.sh's own P/E-core detection step).
#
# Called on EVERY run of this script — fresh install AND update alike —
# so a fixed detector, a hardware change, or simply re-running the
# bootstrap script is enough to correct a bad reading, with no fresh
# enrollment token required. Persisting into the unit file (rather than
# leaving these only exported in the one-time `enroll` shell) is what
# lets detectPECores() see the same value on every later `teepin-agent
# run` too — see that function's own doc comment in cmd/teepin-agent.
apply_pe_core_env() {
    local unit_file="$1"
    [ -n "${TEEPIN_PCORES:-}" ] && [ -n "${TEEPIN_ECORES:-}" ] || return 0
    [ -f "$unit_file" ] || return 0

    info "recording P/E-core split in $unit_file (P=$TEEPIN_PCORES E=$TEEPIN_ECORES)..."
    local tmp
    tmp="$(mktemp)"
    # Strip any TEEPIN_PCORES/TEEPIN_ECORES lines a PREVIOUS run of this
    # function wrote, then insert the current values fresh right after
    # [Service] — idempotent regardless of how many times this runs.
    grep -v -E '^Environment=TEEPIN_(P|E)CORES=' "$unit_file" > "$tmp"
    awk -v p="$TEEPIN_PCORES" -v e="$TEEPIN_ECORES" '
        { print }
        /^\[Service\]/ { print "Environment=TEEPIN_PCORES=" p; print "Environment=TEEPIN_ECORES=" e }
    ' "$tmp" > "$unit_file"
    rm -f "$tmp"
    systemctl daemon-reload
}

# ensure_ecr_pull_secret (re)installs the ECR pull-secret refresh timer and,
# if a credential is already on file, forces one immediate refresh. Called
# on EVERY run of this script — fresh install and update mode alike — so
# the timer can never again silently regress into a one-off nobody
# remembers to make recurring. Found live 2026-09-07: srialla's pull secret
# was a single manual `--once` run from 2026-08-23 with no timer ever
# installed; it silently went stale after 12 hours and stayed that way for
# two weeks before a real Kumbha deploy failed to pull its image with a 403.
#
# Best-effort throughout: a problem here must never fail the agent
# install/update itself — the agent's OWN image pull (for the agent binary
# this script just installed) does not depend on this secret at all, only a
# LATER Kumbha deploy to this node does, so failing loudly-but-not-fatally
# here is strictly better than either silently skipping it or blocking
# everything else over a credential that can be fixed after the fact.
ensure_ecr_pull_secret() {
    local ecr_script
    ecr_script="$(dirname "$0")/refresh-ecr-pull-secret.sh"
    if [ ! -f "$ecr_script" ]; then
        info "WARNING: refresh-ecr-pull-secret.sh not found next to this script — skipping ECR pull-secret setup. Kumbha deploys to this node may fail to pull images with a 403."
        return
    fi

    info "ensuring the ECR pull-secret refresh timer is installed..."
    if ! bash "$ecr_script" --install \
        --account-id "$ECR_ACCOUNT_ID" --region "$ECR_REGION" \
        --secret-name teepin-kumbha-ecr --namespace default; then
        info "WARNING: ECR pull-secret timer setup failed (see the error above) — Kumbha deploys to this node may fail to pull images."
        return
    fi

    local creds_file=/etc/teepin/kumbha-ecr-puller.env
    if [ -s "$creds_file" ] && grep -q "^AWS_ACCESS_KEY_ID=.\+" "$creds_file" 2>/dev/null; then
        info "credential file already populated — forcing an immediate refresh to confirm it works..."
        systemctl start teepin-kumbha-ecr-refresh.service \
            || info "WARNING: the ECR pull-secret refresh failed to run — check: journalctl -u teepin-kumbha-ecr-refresh.service -n 50"
    else
        info "IMPORTANT: $creds_file has no AWS credentials yet — Kumbha image pulls to this node WILL fail (403) until you populate it:"
        info "  aws iam create-access-key --user-name teepin-kumbha-ecr-puller-<env>"
        info "  sudo nano $creds_file   # then: sudo systemctl start teepin-kumbha-ecr-refresh.service"
    fi
}

# --- preconditions -------------------------------------------------------
[ "$(uname -s)" = "Linux" ] || fail "this installer runs on Linux only. On Windows use bootstrap-windows.ps1 (WSL2); on macOS use bootstrap-macos.sh (Lima VM)."
[ "$(id -u)" = "0" ] || fail "run as root (sudo)."

# --- update mode -----------------------------------------------------------
# An already-enrolled node (config + systemd unit already present) re-running
# this script is updating the agent binary, not installing fresh — token and
# control-plane are neither required nor consulted, and k3s / enrollment are
# left untouched. This is what closes the gap that bit us in practice: a bare
# `cp` over a running binary fails with "Text file busy" (the kernel refuses
# to overwrite an inode an active process still maps), so the service MUST be
# stopped before the binary is replaced and started again after.
CONFIG_FILE_DEFAULT=/etc/teepin/agent.json
UNIT_FILE=/etc/systemd/system/teepin-agent.service
if [ -f "$CONFIG_FILE_DEFAULT" ] && [ -f "$UNIT_FILE" ]; then
    info "existing enrollment found ($CONFIG_FILE_DEFAULT) — updating the agent binary only."
    AGENT_BIN=/usr/local/bin/teepin-agent

    if [ -n "$BINARY" ]; then
        info "installing agent from $BINARY"
        NEW_BIN="$BINARY"
    elif [ -f "$(dirname "$0")/teepin-agent" ]; then
        info "installing agent bundled next to this script"
        NEW_BIN="$(dirname "$0")/teepin-agent"
    else
        command -v go >/dev/null 2>&1 || fail "no --binary given and 'go' is not installed to build one. Install Go, or pass --binary."
        ARCH="$(uname -m)"
        case "$ARCH" in
            x86_64)  GOARCH=amd64 ;;
            aarch64|arm64) GOARCH=arm64 ;;
            *) fail "unsupported architecture: $ARCH" ;;
        esac
        info "building agent from source..."
        repo_root="$(cd "$(dirname "$0")/../.." && pwd)"
        NEW_BIN="$(mktemp)"
        ( cd "$repo_root" && GOARCH="$GOARCH" go build -o "$NEW_BIN" ./cmd/teepin-agent ) \
            || fail "agent build failed"
    fi

    info "stopping the agent (the running binary's inode must be free before it can be replaced)..."
    systemctl stop teepin-agent.service

    install -m 0755 "$NEW_BIN" "$AGENT_BIN"

    apply_pe_core_env "$UNIT_FILE"

    info "starting the agent..."
    systemctl start teepin-agent.service

    ensure_ecr_pull_secret

    info "done. Check status with:  systemctl status teepin-agent"
    info "confirm the new binary took effect via the control centre (Nodes) within ~30s."
    exit 0
fi

# --- fresh install ---------------------------------------------------------
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

apply_pe_core_env /etc/systemd/system/teepin-agent.service

info "starting the agent service..."
systemctl daemon-reload
systemctl enable --now teepin-agent.service

ensure_ecr_pull_secret

info "done. The node should appear in the control centre (Nodes) as online within a minute."
info "check status with:  systemctl status teepin-agent"
info "follow logs with:   journalctl -u teepin-agent -f"
