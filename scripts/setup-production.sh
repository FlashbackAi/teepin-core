#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# Complete Production Setup Script for TEEPIN Platform
# Run this on a fresh bare metal server with Ubuntu 22.04 LTS

# This is a bash script: re-exec with bash when invoked via `sh`
# (dash lacks $EUID, read -s, arrays, ...).
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

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

# The repo this script lives in is the deployment source — no separate
# /opt/teepin clone, no wrong-cwd surprises for the sealed-secrets step.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Make kubectl/helm/RKE2 tooling available in THIS shell and every
# future login shell (profile.d, idempotent — unlike .bashrc appends).
# Done first so a re-run after a reboot has a working kubectl at once.
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
export PATH=$PATH:/var/lib/rancher/rke2/bin
cat > /etc/profile.d/teepin-k8s.sh <<'PROFILE'
export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
export PATH=$PATH:/var/lib/rancher/rke2/bin
PROFILE

echo "TEEPIN Production Setup"
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
    pciutils \
    apt-transport-https \
    ca-certificates \
    software-properties-common \
    gnupg \
    lsb-release
# pciutils provides lspci — without it GPU detection silently fails and
# a GPU server would be misconfigured as GPU-less.

# Helm is needed by BOTH the GPU Operator (Step 4) and Ingress NGINX
# (Step 6) — it must be installed unconditionally, not only on GPU nodes.
if ! command -v helm > /dev/null; then
    log_info "Installing Helm..."
    curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
fi

log_info "Prerequisites installed"

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
log_info "Waiting for RKE2 to be ready (this may take 2-3 minutes)..."
sleep 60

# kubectl env was exported at script start (and persisted via
# /etc/profile.d/teepin-k8s.sh).

# Wait for nodes to be ready
until kubectl get nodes 2>/dev/null; do
    log_info "Waiting for Kubernetes API..."
    sleep 5
done

kubectl wait --for=condition=Ready nodes --all --timeout=300s

log_info "RKE2 Kubernetes installed and ready"

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

    # GPU cloud providers usually preinstall the driver; running
    # ubuntu-drivers on top of a vendor CUDA repo only produces apt
    # dependency conflicts. Install only when no working driver exists.
    if command -v nvidia-smi > /dev/null && nvidia-smi > /dev/null 2>&1; then
        log_info "NVIDIA driver already working ($(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -1)) — skipping driver install"
    else
        ubuntu-drivers autoinstall

        if nvidia-smi; then
            log_info "NVIDIA drivers installed successfully"
        else
            log_error "NVIDIA driver installation failed"
            exit 1
        fi
    fi
fi

# ============================================================================
# Step 4: Install NVIDIA GPU Operator (if GPU present)
# ============================================================================
if [ "$GPU_PRESENT" = true ]; then
    log_info "Step 4/10: Installing NVIDIA GPU Operator..."

    # Add NVIDIA Helm repo (Helm itself is installed in Step 1)
    helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
    helm repo update

    # Install GPU Operator
    helm upgrade --install gpu-operator nvidia/gpu-operator \
        --namespace gpu-operator \
        --create-namespace \
        --set mig.strategy=mixed \
        --wait

    # Verify GPU is visible to Kubernetes
    kubectl wait --for=condition=ready pod -l app=nvidia-device-plugin-daemonset -n gpu-operator --timeout=300s

    # Check GPU
    kubectl get nodes -o json | jq '.items[].status.capacity | select(.["nvidia.com/gpu"] != null)'

    log_info "NVIDIA GPU Operator installed"

    # ------------------------------------------------------------------
    # MIG partitioning: mig.strategy=mixed alone creates ZERO MIG
    # devices — MIG-capable nodes must be told which layout to apply.
    # "all-balanced" gives a mix of small/medium/large slices (on an
    # A100/H100 80GB: 2x 1g.10gb + 1x 2g.20gb + 1x 3g.40gb), which
    # exercises every TEEPIN allocation path. Non-MIG GPUs (L40S, A40)
    # skip this and serve whole-GPU allocations.
    # ------------------------------------------------------------------
    MIG_LAYOUT="${MIG_CONFIG:-all-balanced}"
    log_info "Configuring MIG partitioning (layout: $MIG_LAYOUT)..."

    # Wait for MIG extended resources to appear on any node.
    # $1 = timeout in seconds. Returns 0 when devices exist, 2 when the
    # mig-manager reports 'failed' (fail fast), 1 on timeout.
    wait_for_mig_devices() {
        local deadline=$(( $(date +%s) + $1 ))
        local state
        while [ "$(date +%s)" -lt "$deadline" ]; do
            if kubectl get nodes -o json | jq -e \
                '[.items[].status.allocatable | keys[] | select(startswith("nvidia.com/mig-"))] | length > 0' > /dev/null; then
                return 0
            fi
            state=$(kubectl get nodes -l nvidia.com/mig.capable=true \
                -o jsonpath='{.items[0].metadata.labels.nvidia\.com/mig\.config\.state}' 2>/dev/null || true)
            if [ "$state" = "failed" ]; then
                return 2
            fi
            sleep 10
        done
        return 1
    }

    apply_mig_label() {
        kubectl label nodes -l nvidia.com/mig.capable=true \
            nvidia.com/mig.config="$MIG_LAYOUT" --overwrite
    }

    # The mig-manager only reacts to label CHANGES — after a 'failed'
    # state or a reboot, the label must be toggled to force a retry.
    retrigger_mig_manager() {
        log_info "Retriggering the mig-manager (label toggle)..."
        kubectl label nodes -l nvidia.com/mig.capable=true nvidia.com/mig.config- --overwrite
        sleep 5
        apply_mig_label
    }

    show_mig_devices() {
        log_info "MIG devices available:"
        kubectl get nodes -o json | jq '.items[].status.allocatable | with_entries(select(.key | startswith("nvidia.com/")))'
    }

    prompt_reboot_for_mig() {
        log_warn "MIG mode change is PENDING and requires a reboot to apply"
        log_warn "(the GPU reset fails on VMs: the device is held by a host process the mig-manager cannot stop)"
        log_warn "After the reboot, re-run this script — completed steps skip through in seconds."
        if [ "${AUTO_REBOOT:-}" = "true" ]; then
            log_info "AUTO_REBOOT=true — rebooting now"
            reboot
            exit 0
        fi
        read -p "Reboot now? (yes/no): " DO_REBOOT
        if [ "$DO_REBOOT" = "yes" ]; then
            log_info "Rebooting — re-run this script when the server is back (SSH drops for ~2-3 minutes)"
            reboot
            exit 0
        fi
        log_error "Cannot continue without MIG devices — reboot and re-run this script"
        exit 1
    }

    MIG_NODES=$(kubectl get nodes -l nvidia.com/mig.capable=true -o name)
    if [ -z "$MIG_NODES" ]; then
        log_warn "No MIG-capable GPU detected — whole-GPU allocations only"
    else
        apply_mig_label

        log_info "Waiting for MIG devices to appear (mig-manager reconfigures the GPU)..."
        set +e; wait_for_mig_devices 600; MIG_WAIT=$?; set -e

        if [ "$MIG_WAIT" = "0" ]; then
            show_mig_devices
        else
            log_warn "MIG devices not up yet — diagnosing..."
            echo "---- nvidia-mig-manager logs (last 15 lines) ----"
            kubectl logs -n gpu-operator -l app=nvidia-mig-manager --tail=15 2>/dev/null || true
            echo "-------------------------------------------------"

            MIG_CURRENT=$(nvidia-smi -i 0 --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null || echo unknown)
            log_info "MIG mode currently: $MIG_CURRENT"

            if [ "$MIG_CURRENT" = "Disabled" ]; then
                # Known VM failure: enabling MIG needs a GPU reset but a
                # host process holds the device. Try the graceful path
                # first — stop GPU host services, retry the enable — and
                # only fall back to a reboot when that doesn't stick.
                log_info "Trying to enable MIG mode without a reboot (stopping GPU host services)..."
                systemctl stop nvidia-persistenced nvidia-fabricmanager nvidia-dcgm 2>/dev/null || true
                nvidia-smi -i 0 -mig 1 > /dev/null 2>&1 || true
                MIG_CURRENT=$(nvidia-smi -i 0 --query-gpu=mig.mode.current --format=csv,noheader 2>/dev/null || echo unknown)
                systemctl start nvidia-persistenced nvidia-fabricmanager 2>/dev/null || true
            fi

            if [ "$MIG_CURRENT" = "Enabled" ]; then
                # Mode is on (live-enabled just now, or since a reboot) —
                # the layout simply needs a fresh trigger.
                retrigger_mig_manager
                set +e; wait_for_mig_devices 600; MIG_WAIT=$?; set -e
                if [ "$MIG_WAIT" = "0" ]; then
                    show_mig_devices
                else
                    log_error "MIG devices still not available after retrigger:"
                    kubectl logs -n gpu-operator -l app=nvidia-mig-manager --tail=30 2>/dev/null || true
                    exit 1
                fi
            elif [ "$MIG_CURRENT" = "Disabled" ]; then
                prompt_reboot_for_mig
            else
                log_error "Could not determine MIG mode (nvidia-smi said: $MIG_CURRENT)"
                exit 1
            fi
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

log_info "cert-manager installed"

# ============================================================================
# Step 6: Install Ingress NGINX
# ============================================================================
log_info "Step 6/10: Installing Ingress NGINX..."

helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx
helm repo update

# Service type: LoadBalancer requires a provider (MetalLB or a cloud LB)
# — without one, helm --wait hangs forever waiting for an external IP.
# Default to NodePort on fixed ports (30080/30443) which works on any
# bare metal; set INGRESS_SERVICE_TYPE=LoadBalancer once MetalLB exists.
INGRESS_SERVICE_TYPE="${INGRESS_SERVICE_TYPE:-NodePort}"
log_info "Ingress service type: $INGRESS_SERVICE_TYPE"

# helm upgrade --install makes re-runs idempotent (a previously failed
# release would otherwise abort with 'cannot re-use a name').
helm upgrade --install ingress-nginx ingress-nginx/ingress-nginx \
    --namespace ingress-nginx \
    --create-namespace \
    --set controller.service.type="$INGRESS_SERVICE_TYPE" \
    --set controller.service.nodePorts.http=30080 \
    --set controller.service.nodePorts.https=30443 \
    --wait --timeout 10m

kubectl wait --for=condition=available --timeout=300s deployment/ingress-nginx-controller -n ingress-nginx

log_info "Ingress NGINX installed"

# ============================================================================
# Step 7: Install Sealed Secrets
# ============================================================================
log_info "Step 7/10: Installing Sealed Secrets..."

kubectl apply -f https://github.com/bitnami-labs/sealed-secrets/releases/download/v0.24.0/controller.yaml
kubectl wait --for=condition=available --timeout=120s deployment/sealed-secrets-controller -n kube-system

# kubeseal CLI (needed to create sealed secrets in Step 9)
if ! command -v kubeseal > /dev/null; then
    log_info "Installing kubeseal CLI..."
    KUBESEAL_VERSION="0.24.0"
    wget -q "https://github.com/bitnami-labs/sealed-secrets/releases/download/v${KUBESEAL_VERSION}/kubeseal-${KUBESEAL_VERSION}-linux-amd64.tar.gz" -O /tmp/kubeseal.tar.gz
    tar -xzf /tmp/kubeseal.tar.gz -C /tmp kubeseal
    install -m 755 /tmp/kubeseal /usr/local/bin/kubeseal
    rm -f /tmp/kubeseal.tar.gz /tmp/kubeseal
fi

log_info "Sealed Secrets installed (controller + kubeseal CLI)"

# ============================================================================
# Step 8: Install Monitoring Stack (Prometheus + Grafana)
# ============================================================================
log_info "Step 8/10: Installing monitoring stack..."

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# No default credentials: generate a Grafana admin password unless provided.
GRAFANA_PASSWORD="${GRAFANA_PASSWORD:-$(openssl rand -base64 16)}"

helm upgrade --install prometheus prometheus-community/kube-prometheus-stack \
    --namespace monitoring \
    --create-namespace \
    --set prometheus.prometheusSpec.retention=30d \
    --set prometheus.prometheusSpec.resources.requests.memory=2Gi \
    --set grafana.adminPassword="$GRAFANA_PASSWORD" \
    --wait --timeout 15m

log_info "Grafana admin password: $GRAFANA_PASSWORD — SAVE THIS to your password manager"

log_info "Monitoring stack installed"

# ============================================================================
# Step 9: Clone TEEPIN Repository and Deploy Platform
# ============================================================================
log_info "Step 9/10: Deploying TEEPIN platform..."

# Deploy from the repo this script lives in — no separate /opt/teepin
# clone that can drift from the working checkout.
cd "$REPO_DIR"
log_info "Deploying from: $REPO_DIR"

# Create production namespace
kubectl apply -f deploy/production/namespace.yaml

# Sealed secrets: create them interactively right here if no secret
# manifests exist (the controller and kubeseal were installed in Step 7).
if ! ls deploy/production/secrets/*.yaml > /dev/null 2>&1; then
    log_info "No sealed secrets found — creating them now (interactive)"
    bash "$SCRIPT_DIR/create-sealed-secrets.sh"
fi

kubectl apply -f deploy/production/secrets/

# Sealed secrets are encrypted against THIS cluster's controller key.
# Files carried over from a previous cluster apply cleanly but never
# unseal — verify the plain Secrets actually materialize before
# deploying an API server that would crash-loop without them.
log_info "Verifying sealed secrets unseal on this cluster..."
secrets_unsealed() {
    kubectl -n teepin-prod get secret postgresql-credentials > /dev/null 2>&1
}
UNSEALED=false
for i in $(seq 1 12); do
    if secrets_unsealed; then UNSEALED=true; break; fi
    sleep 5
done

if [ "$UNSEALED" != "true" ]; then
    log_warn "Sealed secrets did not unseal — they were likely created for a DIFFERENT cluster"
    log_info "Re-creating sealed secrets for this cluster..."
    rm -f deploy/production/secrets/*.yaml
    bash "$SCRIPT_DIR/create-sealed-secrets.sh"
    kubectl apply -f deploy/production/secrets/
    for i in $(seq 1 12); do
        if secrets_unsealed; then UNSEALED=true; break; fi
        sleep 5
    done
    if [ "$UNSEALED" != "true" ]; then
        log_error "Secrets still not unsealing — check: kubectl logs -n kube-system -l name=sealed-secrets-controller"
        exit 1
    fi
fi
log_info "Sealed secrets applied and unsealed"

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

log_info "API server image built and imported"

# Deploy API server. The image tag never changes (latest + IfNotPresent),
# so on re-runs `apply` alone would keep old pods running the old image —
# always restart the rollout after importing a fresh build.
kubectl apply -f deploy/production/api-server.yaml
kubectl -n teepin-prod rollout restart deployment/api-server

# Wait for API server
kubectl -n teepin-prod rollout status deployment/api-server --timeout=300s

# Apply network policies
kubectl apply -f deploy/production/network-policies.yaml

log_info "TEEPIN platform deployed"

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
echo "=================================================="
echo " TEEPIN Production Setup Complete"
echo "=================================================="
echo ""
log_info "Platform Status:"
echo "  Kubernetes:   $(kubectl version --short | grep Server | awk '{print $3}')"
echo "  API Server:   http://$EXTERNAL_IP"
echo "  Grafana:      http://$EXTERNAL_IP:3000 (admin/admin)"
echo "  GPU:          $([ "$GPU_PRESENT" = true ] && echo "Available" || echo "Not detected")"
echo ""
log_info "Next Steps:"
echo "  1. Run the smoke test:  bash $SCRIPT_DIR/production-smoke-test.sh"
echo "  2. Configure DNS: Point api.teepin.io to $EXTERNAL_IP"
echo "  3. Verify health: curl http://$EXTERNAL_IP/health"
echo "  4. Run security audit: bash $SCRIPT_DIR/run-security-audit.sh"
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
