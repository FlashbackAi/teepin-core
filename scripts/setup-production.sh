#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# Complete Production Setup Script for TEEPIN Platform
# Run this on a fresh bare metal server with Ubuntu 22.04 LTS

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if running as root
if [ "$EUID" -ne 0 ]; then
    log_error "Please run as root (sudo ./setup-production.sh)"
    exit 1
fi

echo "🚀 TEEPIN Production Setup"
echo "=========================="
echo ""
log_info "This will install TEEPIN platform on this server"
log_info "Server: $(hostname)"
log_info "IP: $(hostname -I | awk '{print $1}')"
echo ""

read -p "Continue with production setup? (yes/no): " CONFIRM
if [ "$CONFIRM" != "yes" ]; then
    log_warn "Setup cancelled"
    exit 0
fi

# ============================================================================
# Step 1: System Prerequisites
# ============================================================================
log_info "Step 1/10: Installing system prerequisites..."

apt-get update
apt-get install -y \
    curl \
    wget \
    git \
    jq \
    apt-transport-https \
    ca-certificates \
    software-properties-common \
    gnupg \
    lsb-release

log_info "✅ Prerequisites installed"

# ============================================================================
# Step 2: Install RKE2 (Production Kubernetes)
# ============================================================================
log_info "Step 2/10: Installing RKE2 Kubernetes..."

# Install RKE2
curl -sfL https://get.rke2.io | INSTALL_RKE2_VERSION=v1.28.5+rke2r1 sh -

# Create RKE2 config directory
mkdir -p /etc/rancher/rke2

# Create RKE2 configuration
cat <<EOF > /etc/rancher/rke2/config.yaml
# RKE2 Production Configuration
write-kubeconfig-mode: "0644"
cni: cilium
disable:
  - rke2-ingress-nginx  # We'll install our own
tls-san:
  - $(hostname -I | awk '{print $1}')
  - api.teepin.io
EOF

# Enable and start RKE2
systemctl enable rke2-server.service
systemctl start rke2-server.service

# Wait for RKE2 to be ready
log_info "⏳ Waiting for RKE2 to be ready (this may take 2-3 minutes)..."
sleep 60

# Set up kubectl
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
export PATH=$PATH:/var/lib/rancher/rke2/bin

# Add to bashrc for persistence
echo "export KUBECONFIG=/etc/rancher/rke2/rke2.yaml" >> /root/.bashrc
echo "export PATH=\$PATH:/var/lib/rancher/rke2/bin" >> /root/.bashrc

# Wait for nodes to be ready
until kubectl get nodes 2>/dev/null; do
    log_info "Waiting for Kubernetes API..."
    sleep 5
done

kubectl wait --for=condition=Ready nodes --all --timeout=300s

log_info "✅ RKE2 Kubernetes installed and ready"

# ============================================================================
# Step 3: Install NVIDIA Drivers
# ============================================================================
log_info "Step 3/10: Installing NVIDIA GPU drivers..."

# Check if GPU is present
if ! lspci | grep -i nvidia > /dev/null; then
    log_warn "No NVIDIA GPU detected - skipping GPU setup"
    GPU_PRESENT=false
else
    GPU_PRESENT=true

    # Install NVIDIA drivers
    ubuntu-drivers autoinstall

    # Verify installation
    if nvidia-smi; then
        log_info "✅ NVIDIA drivers installed successfully"
    else
        log_error "NVIDIA driver installation failed"
        exit 1
    fi
fi

# ============================================================================
# Step 4: Install NVIDIA GPU Operator (if GPU present)
# ============================================================================
if [ "$GPU_PRESENT" = true ]; then
    log_info "Step 4/10: Installing NVIDIA GPU Operator..."

    # Install Helm
    curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

    # Add NVIDIA Helm repo
    helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
    helm repo update

    # Install GPU Operator
    helm install gpu-operator nvidia/gpu-operator \
        --namespace gpu-operator \
        --create-namespace \
        --set mig.strategy=mixed \
        --wait

    # Verify GPU is visible to Kubernetes
    kubectl wait --for=condition=ready pod -l app=nvidia-device-plugin-daemonset -n gpu-operator --timeout=300s

    # Check GPU
    kubectl get nodes -o json | jq '.items[].status.capacity | select(.["nvidia.com/gpu"] != null)'

    log_info "✅ NVIDIA GPU Operator installed"

    # ------------------------------------------------------------------
    # MIG partitioning: mig.strategy=mixed alone creates ZERO MIG
    # devices — MIG-capable nodes must be told which layout to apply.
    # "all-balanced" gives a mix of small/medium/large slices (on an
    # A100/H100 80GB: 2x 1g.10gb + 1x 2g.20gb + 1x 3g.40gb), which
    # exercises every TEEPIN allocation path. Non-MIG GPUs (L40S, A40)
    # skip this and serve whole-GPU allocations.
    # ------------------------------------------------------------------
    log_info "Configuring MIG partitioning (layout: ${MIG_CONFIG:-all-balanced})..."

    MIG_NODES=$(kubectl get nodes -l nvidia.com/mig.capable=true -o name)
    if [ -z "$MIG_NODES" ]; then
        log_warn "No MIG-capable GPU detected — whole-GPU allocations only"
    else
        kubectl label nodes -l nvidia.com/mig.capable=true \
            nvidia.com/mig.config="${MIG_CONFIG:-all-balanced}" --overwrite

        log_info "Waiting for MIG devices to appear (mig-manager reconfigures the GPU)..."
        MIG_READY=false
        for i in $(seq 1 60); do
            if kubectl get nodes -o json | jq -e \
                '[.items[].status.allocatable | keys[] | select(startswith("nvidia.com/mig-"))] | length > 0' > /dev/null; then
                MIG_READY=true
                break
            fi
            sleep 10
        done

        if [ "$MIG_READY" = true ]; then
            log_info "✅ MIG devices available:"
            kubectl get nodes -o json | jq '.items[].status.allocatable | with_entries(select(.key | startswith("nvidia.com/")))'
        else
            log_error "MIG devices did not appear within 10 minutes — check: kubectl logs -n gpu-operator -l app=nvidia-mig-manager"
            exit 1
        fi
    fi
else
    log_info "Step 4/10: Skipping GPU Operator (no GPU detected)"
fi

# ============================================================================
# Step 5: Install cert-manager (SSL Certificates)
# ============================================================================
log_info "Step 5/10: Installing cert-manager..."

kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.13.0/cert-manager.yaml
kubectl wait --for=condition=available --timeout=300s deployment/cert-manager -n cert-manager

log_info "✅ cert-manager installed"

# ============================================================================
# Step 6: Install Ingress NGINX
# ============================================================================
log_info "Step 6/10: Installing Ingress NGINX..."

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update

helm install ingress-nginx ingress-nginx/ingress-nginx \
    --namespace ingress-nginx \
    --create-namespace \
    --set controller.service.type=LoadBalancer \
    --wait

kubectl wait --for=condition=available --timeout=300s deployment/ingress-nginx-controller -n ingress-nginx

log_info "✅ Ingress NGINX installed"

# ============================================================================
# Step 7: Install Sealed Secrets
# ============================================================================
log_info "Step 7/10: Installing Sealed Secrets..."

kubectl apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.24.0/controller.yaml
kubectl wait --for=condition=available --timeout=120s deployment/sealed-secrets-controller -n kube-system

log_info "✅ Sealed Secrets installed"

# ============================================================================
# Step 8: Install Monitoring Stack (Prometheus + Grafana)
# ============================================================================
log_info "Step 8/10: Installing monitoring stack..."

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install prometheus prometheus-community/kube-prometheus-stack \
    --namespace monitoring \
    --create-namespace \
    --set prometheus.prometheusSpec.retention=30d \
    --set prometheus.prometheusSpec.resources.requests.memory=2Gi \
    --set grafana.adminPassword=admin \
    --wait

log_info "✅ Monitoring stack installed"

# ============================================================================
# Step 9: Clone TEEPIN Repository and Deploy Platform
# ============================================================================
log_info "Step 9/10: Deploying TEEPIN platform..."

# Clone repo (if not already present)
if [ ! -d "/opt/teepin" ]; then
    cd /opt
    git clone https://github.com/FlashbackAi/teepin-core.git teepin
fi

cd /opt/teepin

# Create production namespace
kubectl apply -f deploy/production/namespace.yaml

# Deploy sealed secrets (must be created first - see scripts/create-sealed-secrets.sh)
if [ -d "deploy/production/secrets" ]; then
    kubectl apply -f deploy/production/secrets/
    log_info "✅ Sealed secrets applied"
else
    log_warn "No sealed secrets found - you must run scripts/create-sealed-secrets.sh first!"
    log_warn "Setup will continue, but API server will fail without secrets"
fi

# Deploy Redis for rate limiting
kubectl apply -f deploy/local/redis.yaml

# Wait for Redis
kubectl wait --for=condition=ready pod -l app=redis -n rate-limit --timeout=120s

# ----------------------------------------------------------------------
# Build the API server image on this host and import it into RKE2's
# containerd. On a single-node bootstrap there is no external registry
# to pull from (Harbor runs inside this same cluster).
# ----------------------------------------------------------------------
log_info "Building TEEPIN API server image..."

if ! command -v docker > /dev/null; then
    log_info "Installing Docker (build tooling)..."
    apt-get install -y docker.io
    systemctl enable --now docker
fi

docker build -t teepin/api-server:latest -f cmd/api-server/Dockerfile .

log_info "Importing image into RKE2 containerd..."
docker save teepin/api-server:latest -o /tmp/teepin-api-server.tar
/var/lib/rancher/rke2/bin/ctr \
    --address /run/k3s/containerd/containerd.sock \
    -n k8s.io images import /tmp/teepin-api-server.tar
rm -f /tmp/teepin-api-server.tar

log_info "✅ API server image built and imported"

# Deploy API server
kubectl apply -f deploy/production/api-server.yaml

# Wait for API server
kubectl wait --for=condition=available --timeout=300s deployment/api-server -n teepin-prod

# Apply network policies
kubectl apply -f deploy/production/network-policies.yaml

log_info "✅ TEEPIN platform deployed"

# ============================================================================
# Step 10: Post-Install Configuration
# ============================================================================
log_info "Step 10/10: Post-install configuration..."

# Database migrations are embedded in the API binary and applied
# automatically at startup (TEEPIN_AUTO_MIGRATE=true by default) — no
# separate migration step is needed. Verify via the API logs:
log_info "Verifying database migrations (applied automatically by the API server)..."
kubectl logs deployment/api-server -n teepin-prod --tail=50 | grep -i "migrat" || \
    log_warn "No migration log lines found yet — check: kubectl logs deployment/api-server -n teepin-prod"

# Get LoadBalancer IP
log_info "Waiting for LoadBalancer IP..."
sleep 10
EXTERNAL_IP=$(kubectl get svc ingress-nginx-controller -n ingress-nginx -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

if [ -z "$EXTERNAL_IP" ]; then
    EXTERNAL_IP=$(hostname -I | awk '{print $1}')
    log_warn "LoadBalancer IP not assigned yet - using server IP: $EXTERNAL_IP"
fi

# ============================================================================
# Installation Complete
# ============================================================================
echo ""
echo "✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅"
echo "🎉 TEEPIN Production Setup Complete!"
echo "✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅✅"
echo ""
log_info "Platform Status:"
echo "  Kubernetes:   $(kubectl version --short | grep Server | awk '{print $3}')"
echo "  API Server:   http://$EXTERNAL_IP"
echo "  Grafana:      http://$EXTERNAL_IP:3000 (admin/admin)"
echo "  GPU:          $([ "$GPU_PRESENT" = true ] && echo "Available" || echo "Not detected")"
echo ""
log_info "Next Steps:"
echo "  1. Configure DNS: Point api.teepin.io to $EXTERNAL_IP"
echo "  2. Create sealed secrets: ./scripts/create-sealed-secrets.sh"
echo "  3. Verify health: curl http://$EXTERNAL_IP/health"
echo "  4. Run security audit: ./scripts/run-security-audit.sh"
echo "  5. Create first customer project"
echo ""
log_info "Useful Commands:"
echo "  kubectl get pods -n teepin-prod           # Check platform pods"
echo "  kubectl logs -f deployment/api-server -n teepin-prod  # API server logs"
echo "  kubectl get nodes -o wide                 # Check nodes"
echo "  nvidia-smi                                # Check GPU"
echo ""
log_info "Documentation: https://docs.teepin.io"
echo ""
