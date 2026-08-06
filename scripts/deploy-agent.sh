#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# Builds the cluster agent and deploys it to the local GPU cluster.
#
# The agent dials OUT to the control plane, so this opens no ports and
# needs no firewall change. It does need the shared token, which must
# match teepin/agent-token in AWS Secrets Manager — the script fetches it
# from there when the AWS CLI is configured, and otherwise reads
# TEEPIN_AGENT_TOKEN.
#
# Usage:
#   bash scripts/deploy-agent.sh
#   TEEPIN_AGENT_TOKEN=xxx bash scripts/deploy-agent.sh   # without AWS creds

if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

NAMESPACE="teepin-system"
IMAGE_NAME="${TEEPIN_AGENT_IMAGE:-teepin/agent}"
CONTROL_PLANE="${TEEPIN_CONTROL_PLANE:-api.teepin.com:443}"

info() { echo "[INFO] $1"; }
warn() { echo "[WARN] $1"; }
fail() { echo "[ERROR] $1" >&2; exit 1; }

command -v kubectl > /dev/null || fail "kubectl not found"

# RKE2 installs kubectl outside the default PATH and writes its
# kubeconfig somewhere root-only; pick both up automatically rather than
# making the operator remember two exports.
if [ -f /etc/rancher/rke2/rke2.yaml ] && [ -z "${KUBECONFIG:-}" ]; then
    export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
    info "Using RKE2 kubeconfig"
fi
export PATH="$PATH:/var/lib/rancher/rke2/bin"

kubectl cluster-info > /dev/null 2>&1 || fail "cannot reach the Kubernetes cluster"

# ---------------------------------------------------------------------
# Token
# ---------------------------------------------------------------------
TOKEN="${TEEPIN_AGENT_TOKEN:-}"

if [ -z "$TOKEN" ] && command -v aws > /dev/null 2>&1; then
    info "Fetching agent token from AWS Secrets Manager..."
    TOKEN=$(aws secretsmanager get-secret-value \
        --secret-id teepin/agent-token \
        --query SecretString --output text 2>/dev/null || true)
fi

[ -n "$TOKEN" ] || fail "no agent token: set TEEPIN_AGENT_TOKEN or configure the AWS CLI"

# ---------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------
VERSION=$(cd "$REPO_ROOT" && git rev-parse --short HEAD 2>/dev/null || echo "dev")
if ! (cd "$REPO_ROOT" && git diff --quiet 2>/dev/null); then
    VERSION="${VERSION}-dirty"
    warn "Working tree has uncommitted changes - tagging as $VERSION"
fi

info "Building agent $VERSION..."
cd "$REPO_ROOT"
docker build \
    --build-arg "VERSION=$VERSION" \
    -t "${IMAGE_NAME}:${VERSION}" \
    -t "${IMAGE_NAME}:latest" \
    -f cmd/teepin-agent/Dockerfile .

# RKE2 uses containerd, not Docker, so a locally-built image is invisible
# to it until imported. Skipping this is the classic "ImagePullBackOff on
# an image that definitely exists" failure.
if command -v /var/lib/rancher/rke2/bin/ctr > /dev/null 2>&1; then
    info "Importing image into containerd..."
    docker save "${IMAGE_NAME}:${VERSION}" | \
        /var/lib/rancher/rke2/bin/ctr \
            --address /run/k3s/containerd/containerd.sock \
            --namespace k8s.io images import -
fi

# ---------------------------------------------------------------------
# Deploy
# ---------------------------------------------------------------------
info "Applying manifests..."
kubectl apply -f "$REPO_ROOT/deploy/agent/agent.yaml"

info "Updating token secret..."
kubectl create secret generic teepin-agent-token \
    --namespace "$NAMESPACE" \
    --from-literal=token="$TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -

# Point the deployment at the freshly built tag and the requested
# control plane. Patching rather than editing the manifest keeps the
# committed YAML free of environment-specific values.
kubectl set image -n "$NAMESPACE" deployment/teepin-agent \
    "agent=${IMAGE_NAME}:${VERSION}"
kubectl set env -n "$NAMESPACE" deployment/teepin-agent \
    "TEEPIN_CONTROL_PLANE=$CONTROL_PLANE"

# A locally-built image has no registry to pull from.
kubectl patch deployment teepin-agent -n "$NAMESPACE" --type=strategic -p \
    '{"spec":{"template":{"spec":{"containers":[{"name":"agent","imagePullPolicy":"IfNotPresent"}]}}}}'

kubectl rollout restart -n "$NAMESPACE" deployment/teepin-agent
info "Waiting for rollout..."
kubectl rollout status -n "$NAMESPACE" deployment/teepin-agent --timeout=120s

echo ""
info "Agent deployed: ${IMAGE_NAME}:${VERSION} -> $CONTROL_PLANE"
echo ""
echo "Follow the connection:"
echo "  kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=teepin-agent -f"
echo ""
echo "Expected within a few seconds:"
echo "  Connected to Kubernetes"
echo "  Connected to control plane at $CONTROL_PLANE as provider primary"
