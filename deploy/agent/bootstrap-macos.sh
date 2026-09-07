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

# Reserved for the HOST OS and the operator's own concurrent use of their
# own machine — a FIXED amount, not a proportional split (e.g. half).
# A proportional split wastes enormous headroom on a large machine (a
# 64GB Mac keeping 32GB idle "just in case" makes no sense); a fixed
# reservation is also the actual constraint here — this is not an AWS
# EC2 host (dedicated, no other user, nearly the whole machine handed to
# the guest by design), it is the operator's OWN daily computer, running
# alongside their own use of it while it also rents out capacity.
OS_RESERVED_MEM_GIB=4
OS_RESERVED_CPUS=2

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
# Homebrew refuses outright to run as root (it manages an unprivileged,
# user-owned install), and Lima manages its VM state under the invoking
# user's own home directory — both need to run as your normal account, not
# root. The one place this script genuinely needs root is INSIDE the VM,
# already scoped correctly below via its own `sudo bash install.sh` call —
# running the whole script as root does not grant that, it just breaks
# Homebrew before getting there. A root shell here almost always means
# `sudo -s`/`su` was run first; exit that and re-run as yourself.
[ "$(id -u)" -ne 0 ] || fail "do not run this as root (or via sudo/sudo -s/su) — Homebrew and Lima must run as your normal user. install.sh's own sudo call, inside the VM, is already scoped correctly and needs no help from a root shell out here."
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
# Size the VM to the HOST's real capacity minus a fixed reservation for
# the operator's own use (see OS_RESERVED_* above) — Lima's own
# template://ubuntu default is a small, generic 4 CPUs/4GiB regardless of
# the actual machine, which badly under-reports a real Mac's capacity
# (confirmed live: an M4 Mac Mini showed as "4 vCPU · 4 GB" in the
# control centre). sysctl reads the TRUE host values directly; nothing
# here depends on what Lima's template happened to default to.
total_mem_gib=$(( $(sysctl -n hw.memsize) / 1024 / 1024 / 1024 ))
total_cpus=$(sysctl -n hw.ncpu)
vm_mem_gib=$(( total_mem_gib - OS_RESERVED_MEM_GIB ))
vm_cpus=$(( total_cpus - OS_RESERVED_CPUS ))
# Floors guard a very small or unusual host from computing to zero/negative
# — not expected on real Mac hardware, but a VM with 0 of either would
# simply fail to start, which is a worse failure mode than a small floor.
[ "$vm_mem_gib" -ge 2 ] || vm_mem_gib=2
[ "$vm_cpus" -ge 1 ] || vm_cpus=1
info "host: ${total_cpus} CPUs / ${total_mem_gib}GiB -> VM: ${vm_cpus} CPUs / ${vm_mem_gib}GiB (${OS_RESERVED_CPUS} CPUs / ${OS_RESERVED_MEM_GIB}GiB reserved for the OS)"

# Start (or reuse) a VM that starts on login, so the node survives reboots as
# long as the user logs in. Uses the default Ubuntu template.
if limactl list --quiet 2>/dev/null | grep -qx "$VM_NAME"; then
    info "Lima VM '$VM_NAME' exists; ensuring it is running..."
    # Best-effort resize of an ALREADY-EXISTING VM (e.g. one created before
    # this sizing logic existed, still sitting at Lima's small default).
    # `limactl edit` applies to the instance's stored config, taking effect
    # on next start — requires the VM to be stopped first. Not fatal if the
    # installed Lima version does not support this flag combination: the
    # VM still starts at whatever size it already has, and the message
    # below tells the operator how to force a resize.
    limactl stop "$VM_NAME" >/dev/null 2>&1 || true
    if ! limactl edit "$VM_NAME" --cpus "$vm_cpus" --memory "${vm_mem_gib}GiB" >/dev/null 2>&1; then
        info "note: could not resize the existing VM automatically (older Lima version?)."
        info "To apply the new sizing, run: limactl delete $VM_NAME   then re-run this script."
    fi
    limactl start "$VM_NAME" || true
else
    info "creating Lima VM '$VM_NAME'..."
    limactl start --name "$VM_NAME" --cpus "$vm_cpus" --memory "${vm_mem_gib}GiB" template://ubuntu
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
