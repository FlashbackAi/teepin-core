#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

# Install Sealed Secrets for encrypting Kubernetes secrets
# Usage: ./scripts/install-sealed-secrets.sh

set -e

echo "🔒 Installing Sealed Secrets..."

# Install Sealed Secrets controller
kubectl apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.24.0/controller.yaml

# Wait for controller to be ready
echo "⏳ Waiting for Sealed Secrets controller to be ready..."
kubectl wait --for=condition=available --timeout=120s \
  deployment/sealed-secrets-controller -n kube-system

# Install kubeseal CLI (if not already installed)
if ! command -v kubeseal &> /dev/null; then
    echo "📦 Installing kubeseal CLI..."

    # Detect OS
    OS=$(uname -s | tr '[:upper:]' '[:lower:]')
    ARCH=$(uname -m)

    if [ "$ARCH" = "x86_64" ]; then
        ARCH="amd64"
    elif [ "$ARCH" = "aarch64" ]; then
        ARCH="arm64"
    fi

    # Download kubeseal
    KUBESEAL_VERSION="0.24.0"
    wget "https://github.com/bitnami-labs/sealed-secrets/releases/download/v${KUBESEAL_VERSION}/kubeseal-${KUBESEAL_VERSION}-${OS}-${ARCH}.tar.gz" -O /tmp/kubeseal.tar.gz

    # Extract and install
    tar -xzf /tmp/kubeseal.tar.gz -C /tmp
    sudo install -m 755 /tmp/kubeseal /usr/local/bin/kubeseal
    rm /tmp/kubeseal.tar.gz /tmp/kubeseal

    echo "✅ kubeseal CLI installed"
else
    echo "✅ kubeseal CLI already installed"
fi

# Verify installation
echo ""
echo "🔍 Verifying installation..."
kubectl get pods -n kube-system -l name=sealed-secrets-controller
echo ""
kubeseal --version

echo ""
echo "✅ Sealed Secrets installed successfully!"
echo ""
echo "Usage:"
echo "  # Create a sealed secret from a regular secret:"
echo "  kubectl create secret generic mysecret --dry-run=client -o yaml \\"
echo "    --from-literal=password=supersecret | \\"
echo "    kubeseal -o yaml > mysealedsecret.yaml"
echo ""
echo "  # Apply the sealed secret (safe to commit to Git):"
echo "  kubectl apply -f mysealedsecret.yaml"
echo ""
