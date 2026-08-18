#!/usr/bin/env bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# TEEPIN home-node bootstrap for macOS.
#
# The agent runs inside Linux. On macOS that Linux is a lightweight VM managed
# by Lima. This script ensures Lima + a Linux VM, then runs the Linux core
# installer (install.sh) INSIDE the VM. The node is CPU-only (the VM does not
# reach the Mac's GPU).
#
# Usage:
#   # First install:
#   bash bootstrap-macos.sh --token <tne_...> --control-plane https://api.teepin.com
#
#   # Update an already-enrolled node's agent binary (no token needed --
#   # install.sh detects the existing enrollment inside the VM itself):
#   bash bootstrap-macos.sh
#
# Requires Homebrew (to install Lima). The VM's arch matches the Mac (arm64 on
# Apple Silicon, amd64 on Intel), which is what the node reports.

set -euo pipefail

TOKEN=""
CONTROL_PLANE=""
GRPC_ADDR=""
VM_NAME="teepin"

while [ $# -gt 0 ]; do
    case "$1" in
        --token)         TOKEN="$2"; shift 2 ;;
        --control-plane) CONTROL_PLANE="$2"; shift 2 ;;
        --grpc)          GRPC_ADDR="$2"; shift 2 ;;
        --vm)            VM_NAME="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

info() { echo "[bootstrap] $*"; }
fail() { echo "[bootstrap] ERROR: $*" >&2; exit 1; }

[ "$(uname -s)" = "Darwin" ] || fail "this bootstrap is for macOS. On Linux run install.sh directly; on Windows use bootstrap-windows.ps1."
# --token/--control-plane are only required for a first install. A re-run
# against an already-enrolled VM is an update, and install.sh detects that
# itself (existing /etc/teepin/agent.json + systemd unit) -- but whether
# THIS run is fresh or an update depends on the VM this script is about to
# ensure exists below, so the actual requirement check is deferred until
# after that VM is confirmed running (see "install mode" below).

# --- 1. Lima -------------------------------------------------------------
if ! command -v limactl >/dev/null 2>&1; then
    command -v brew >/dev/null 2>&1 || fail "Homebrew is required to install Lima. See https://brew.sh"
    info "installing Lima via Homebrew..."
    brew install lima
fi

# --- 2. Linux VM ---------------------------------------------------------
# Start (or reuse) a VM that starts on login, so the node survives reboots as
# long as the user logs in. Uses the default Ubuntu template.
if limactl list --quiet 2>/dev/null | grep -qx "$VM_NAME"; then
    info "Lima VM '$VM_NAME' exists; ensuring it is running..."
    limactl start "$VM_NAME" || true
else
    info "creating Lima VM '$VM_NAME'..."
    # --plain keeps it a stock Linux; we install everything via install.sh.
    limactl start --name "$VM_NAME" template://ubuntu
fi

# Register the VM to auto-start on login (best effort).
limactl start-at-login "$VM_NAME" >/dev/null 2>&1 || \
    info "note: could not enable start-at-login; start the VM manually after a reboot with 'limactl start $VM_NAME'."

# --- 3. run the Linux core installer inside the VM ----------------------
here="$(cd "$(dirname "$0")" && pwd)"

# install.sh decides fresh-vs-update for itself once it runs (existing
# /etc/teepin/agent.json + systemd unit inside the VM), but --token/
# --control-plane must be validated HERE: a missing --token would otherwise
# surface as install.sh's bash error from inside the VM instead of a clear
# message at this script's own usage boundary.
already_enrolled="$(limactl shell "$VM_NAME" -- bash -c \
    '[ -f /etc/teepin/agent.json ] && [ -f /etc/systemd/system/teepin-agent.service ] && echo yes || echo no' \
    2>/dev/null || echo no)"

if [ "$already_enrolled" = "yes" ]; then
    info "existing enrollment found inside the VM -- updating the agent binary only (--token/--control-plane not needed)."
    install_args=""
else
    [ -n "$TOKEN" ] || fail "--token is required for a first install (no existing enrollment found inside the VM)."
    [ -n "$CONTROL_PLANE" ] || fail "--control-plane is required for a first install (no existing enrollment found inside the VM)."
    grpc_arg=""
    [ -n "$GRPC_ADDR" ] && grpc_arg="--grpc $GRPC_ADDR"
    install_args="--token '$TOKEN' --control-plane '$CONTROL_PLANE' $grpc_arg"
fi

info "running the Linux installer inside the VM..."
# Lima mounts the host home read-only by default; copy the script dir into the
# VM's writable /tmp, then run it there as root.
limactl shell "$VM_NAME" -- bash -c "
    set -e
    rm -rf /tmp/teepin-agent-install && mkdir -p /tmp/teepin-agent-install
    cp -r '$here'/. /tmp/teepin-agent-install/
    sudo bash /tmp/teepin-agent-install/install.sh $install_args
"

info "done. The node should appear in the control centre (Nodes) as online within a minute."
info "check inside the VM:  limactl shell $VM_NAME -- systemctl status teepin-agent"
