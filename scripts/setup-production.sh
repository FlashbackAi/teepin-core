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

# Non-interactive apt: without this, needrestart's "Restarting
# services..." hook can pop an interactive dialog (or hang) after any
# package install and stall the whole setup.
export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a
export NEEDRESTART_SUSPEND=1

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
# profile.d only covers login shells; plain `sudo su` skips it. Hook
# .bashrc too so kubectl works in every interactive root shell.
grep -q "teepin-k8s" /root/.bashrc 2>/dev/null || \
    echo '[ -f /etc/profile.d/teepin-k8s.sh ] && . /etc/profile.d/teepin-k8s.sh' >> /root/.bashrc

# Belt and braces: symlink kubectl into a directory that is on every
# shell's default PATH, so an already-open (stale) shell still works
# and nobody is tempted to `snap install kubectl` — a snap kubectl has
# no kubeconfig and fails against localhost:8080.
if [ -x /var/lib/rancher/rke2/bin/kubectl ] && [ ! -e /usr/local/bin/kubectl ]; then
    ln -sf /var/lib/rancher/rke2/bin/kubectl /usr/local/bin/kubectl
fi

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
# CNI choice. Default is canal (RKE2's own default: Flannel + Calico) —
# Cilium's eBPF datapath proved fragile across several rented GPU hosts
# (CoreDNS unable to reach the API VIP, pods losing egress), and none of
# TEEPIN's networking needs Cilium-specific features. Override with
# CNI=cilium if you specifically want CiliumNetworkPolicy support.
CNI="${CNI:-canal}"
log_info "CNI: $CNI"

cat <<EOF > /etc/rancher/rke2/config.yaml
# RKE2 Production Configuration
write-kubeconfig-mode: "0644"
cni: $CNI
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

# ----------------------------------------------------------------------
# In-cluster DNS sanity. CoreDNS forwards to the node's resolv.conf,
# and on some GPU providers that upstream is flaky or a loopback stub —
# external lookups (e.g. the RDS endpoint) then fail with SERVFAIL
# ("server misbehaving") or hang. Pin CoreDNS to public resolvers when
# external resolution fails from inside the cluster.
# ----------------------------------------------------------------------
# Pod egress first: a pod that cannot reach the outside world at all
# means the CNI datapath is broken (or the provider drops tenant pod
# traffic). Nothing downstream can work, and no amount of DNS or
# secrets config fixes it — fail here, at minute two, not at the smoke
# test an hour later.
log_info "Checking pod network egress..."
if ! kubectl run netcheck --rm -i --restart=Never --image=busybox:1.36 \
    --pod-running-timeout=90s -- ping -c 2 -W 5 8.8.8.8 2>/dev/null | grep -q "bytes from"; then
    log_error "A new pod has NO network egress — the CNI datapath is broken."
    log_error "Diagnose with:"
    log_error "  iptables -S FORWARD | head -3                                  # Docker sets DROP; must be ACCEPT"
    log_error "  kubectl -n kube-system exec ds/cilium -- cilium status --verbose | grep -i masquerad"
    log_error "  kubectl run vipcheck --rm -i --restart=Never --image=busybox:1.36 -- nc -zv -w 5 10.43.0.1 443"
    log_error "If the host has egress but pods do not, and Cilium reports healthy,"
    log_error "the provider is dropping tenant pod traffic — use a different server."
    exit 1
fi
log_info "Pod network egress OK"

# Three consecutive lookups must succeed: providers with a flaky
# resolver often pass a single probe and then SERVFAIL under real load
# ("server misbehaving"), which surfaces much later as database
# connection failures inside the API.
log_info "Checking in-cluster DNS resolution..."
DNS_OK=true
for i in 1 2 3; do
    if ! kubectl run dnscheck-$i --rm -i --restart=Never --image=busybox:1.36 \
        --pod-running-timeout=90s -- nslookup amazonaws.com > /dev/null 2>&1; then
        DNS_OK=false
        break
    fi
done
if [ "$DNS_OK" != "true" ]; then
    log_warn "In-cluster DNS cannot resolve external names — pinning CoreDNS upstream to 8.8.8.8/1.1.1.1"
    kubectl -n kube-system get configmap rke2-coredns-rke2-coredns -o yaml \
        | sed 's|forward[[:space:]]*\.[[:space:]]*/etc/resolv.conf|forward . 8.8.8.8 1.1.1.1|' \
        | kubectl apply -f -
    kubectl -n kube-system rollout restart deployment rke2-coredns-rke2-coredns
    kubectl -n kube-system rollout status deployment rke2-coredns-rke2-coredns --timeout=120s
fi

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

    # Verify GPU is visible to Kubernetes. On a fresh cluster this can
    # take a while: the operator pulls images, validates the driver, and
    # installs the container toolkit (with a containerd restart) before
    # the device plugin can become Ready.
    kubectl wait --for=condition=ready pod -l app=nvidia-device-plugin-daemonset -n gpu-operator --timeout=900s

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

# Sealed secrets: (re)create them interactively when no VALID manifests
# exist. Content check, not just filenames — a failed earlier run can
# leave empty or comment-only .yaml files behind, which `kubectl apply`
# rejects with "no objects passed to apply".
if ! grep -q "kind: SealedSecret" deploy/production/secrets/*.yaml 2>/dev/null; then
    log_info "No valid sealed-secret manifests found — creating them now (interactive)"
    rm -f deploy/production/secrets/*.yaml 2>/dev/null || true
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

# Docker sets the iptables FORWARD policy to DROP on every start
# (including at boot), which silently kills CNI pod traffic. Restore
# ACCEPT now and persist it across reboots.
iptables -P FORWARD ACCEPT
cat > /etc/systemd/system/teepin-forward-accept.service <<'EOF'
[Unit]
Description=Restore iptables FORWARD ACCEPT (Docker sets DROP, breaking CNI pod traffic)
After=docker.service network-online.target
[Service]
Type=oneshot
ExecStart=/usr/sbin/iptables -P FORWARD ACCEPT
[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable teepin-forward-accept.service > /dev/null 2>&1 || true

# Re-verify pod egress: the Docker install above is the single most
# common way a working cluster loses pod networking mid-setup.
if ! kubectl run netcheck2 --rm -i --restart=Never --image=busybox:1.36 \
    --pod-running-timeout=90s -- ping -c 2 -W 5 8.8.8.8 2>/dev/null | grep -q "bytes from"; then
    log_error "Pod egress broke after installing Docker — check: iptables -S FORWARD | head -3"
    exit 1
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
# Network policies: apply the set matching the active CNI. Cilium gets
# the richer CiliumNetworkPolicy (FQDN + L7 rules); every other CNI gets
# the portable standard NetworkPolicy equivalent. Tenant isolation is
# enforced either way.
if kubectl get crd ciliumnetworkpolicies.cilium.io > /dev/null 2>&1; then
    kubectl apply -f deploy/production/network-policies.yaml
    log_info "Network policies applied (CiliumNetworkPolicy)"
else
    kubectl apply -f deploy/production/network-policies-standard.yaml

    # Node IPs are cluster-specific and cannot be hardcoded. On bare
    # metal they are frequently PUBLIC addresses, and kube-proxy DNATs
    # the apiserver VIP to them — so a customer pod can otherwise reach
    # the Kubernetes API through the node address.
    #
    # NetworkPolicy rules are ADDITIVE: a second policy cannot subtract
    # from the first. The node IPs must therefore be merged into the
    # SAME `except` list, patching the policy in place.
    NODE_IPS=$(kubectl get nodes -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{.status.addresses[?(@.type=="ExternalIP")].address}{"\n"}{end}' | grep -v '^$' | sort -u)
    if [ -n "$NODE_IPS" ]; then
        EXCEPT_JSON=$(
            {
                printf '"10.0.0.0/8","172.16.0.0/12","192.168.0.0/16","169.254.0.0/16","100.64.0.0/10"'
                for ip in $NODE_IPS; do printf ',"%s/32"' "$ip"; done
            }
        )
        # Egress rule 0 is DNS, rule 1 is the internet rule being patched.
        kubectl -n default patch networkpolicy customer-instance-isolation --type=json \
            -p "[{\"op\":\"replace\",\"path\":\"/spec/egress/1/to/0/ipBlock/except\",\"value\":[$EXCEPT_JSON]}]" \
            > /dev/null
        log_info "Node IPs excluded from customer egress: $(echo "$NODE_IPS" | tr '\n' ' ')"
    fi

    log_info "Network policies applied (standard NetworkPolicy, CNI=$CNI)"
fi

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
