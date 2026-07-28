#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

# Create Sealed Secrets for production deployment
# This encrypts sensitive data that can be safely committed to Git

set -e

# Always operate from the repo root, no matter where this is invoked
# from — output must land in <repo>/deploy/production/secrets, which is
# where setup-production.sh applies it from.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.."

# RKE2 kubectl env (harmless if kubectl is already on PATH).
if ! command -v kubectl > /dev/null 2>&1; then
    export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
    export PATH=$PATH:/var/lib/rancher/rke2/bin
fi

NAMESPACE="${1:-teepin-prod}"
OUTPUT_DIR="deploy/production/secrets"

echo "Creating Sealed Secrets for namespace: $NAMESPACE"
echo "Output directory: $(pwd)/$OUTPUT_DIR"
echo ""

# Fail early with actionable messages instead of half-writing files.
if ! command -v kubectl > /dev/null 2>&1 || ! kubectl get nodes > /dev/null 2>&1; then
    echo "ERROR: kubectl cannot reach the cluster."
    echo "       Run setup-production.sh first (it installs RKE2 and writes /etc/profile.d/teepin-k8s.sh)."
    exit 1
fi
if ! command -v kubeseal > /dev/null 2>&1; then
    echo "ERROR: kubeseal CLI not found."
    echo "       Run setup-production.sh (Step 7 installs it) or scripts/install-sealed-secrets.sh."
    exit 1
fi
if ! kubectl get deployment sealed-secrets-controller -n kube-system > /dev/null 2>&1; then
    echo "ERROR: sealed-secrets controller is not installed in the cluster."
    echo "       Run setup-production.sh (Step 7) or scripts/install-sealed-secrets.sh."
    exit 1
fi

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Function to create and seal a secret with ALL of its keys in one file.
# Usage: create_sealed_secret <name> <description> key1=value1 [key2=value2 ...]
# (One call per secret — separate calls would overwrite each other's keys.)
create_sealed_secret() {
    local name=$1
    local description=$2
    shift 2

    local literals=()
    for kv in "$@"; do
        literals+=(--from-literal="$kv")
    done

    echo "Creating sealed secret: $name ($# keys)"

    # Create regular secret (dry-run) with every key, then seal it
    kubectl create secret generic "$name" \
        --dry-run=client \
        --namespace="$NAMESPACE" \
        "${literals[@]}" \
        -o yaml | \
    kubeseal \
        --controller-namespace=kube-system \
        --controller-name=sealed-secrets-controller \
        --format=yaml \
        > "$OUTPUT_DIR/$name.yaml"

    # Add description as comment
    sed -i "1i# $description" "$OUTPUT_DIR/$name.yaml"

    echo "  Created: $OUTPUT_DIR/$name.yaml"
}

# PostgreSQL / AWS RDS credentials — verified live before sealing, so a
# typo'd password or missing database is caught HERE, not as a cryptic
# API crash-loop after deployment.
echo ""
echo "PostgreSQL Credentials"
while true; do
    read -p "Enter PostgreSQL host (e.g., xxx.cluster-yyy.us-east-1.rds.amazonaws.com): " PG_HOST
    read -p "Enter PostgreSQL username [default: teepin]: " PG_USER
    PG_USER=${PG_USER:-teepin}
    read -sp "Enter PostgreSQL password: " PG_PASSWORD
    echo ""
    read -p "Enter PostgreSQL database name [default: teepin]: " PG_DATABASE
    PG_DATABASE=${PG_DATABASE:-teepin}

    if ! command -v psql > /dev/null 2>&1; then
        echo "Installing postgresql-client (for connectivity verification)..."
        apt-get install -y -qq postgresql-client > /dev/null
    fi

    echo "Verifying database connectivity..."
    if ! timeout 8 bash -c "cat < /dev/null > /dev/tcp/$PG_HOST/5432" 2>/dev/null; then
        echo "ERROR: cannot reach $PG_HOST:5432 from this server."
        echo "  For AWS RDS/Aurora check:"
        echo "    1. The writer INSTANCE is set to 'Publicly accessible' (Modify -> Connectivity)"
        echo "    2. The security group allows inbound 5432 from this server's IP ($(hostname -I | awk '{print $1}'))"
        read -p "Retry with different values? (yes = retry / no = seal anyway): " RETRY
        [ "$RETRY" = "yes" ] && continue
        echo "WARN: sealing UNVERIFIED database credentials"
        break
    fi

    if ! PGPASSWORD="$PG_PASSWORD" psql "host=$PG_HOST user=$PG_USER dbname=postgres sslmode=require connect_timeout=8" -c "SELECT 1" > /dev/null 2>&1; then
        echo "ERROR: authentication failed for user '$PG_USER' (host reachable, credentials rejected)."
        read -p "Retry with different values? (yes = retry / no = seal anyway): " RETRY
        [ "$RETRY" = "yes" ] && continue
        echo "WARN: sealing UNVERIFIED database credentials"
        break
    fi

    # Fresh RDS/Aurora clusters only have the 'postgres' database —
    # create the application database if it does not exist yet.
    if ! PGPASSWORD="$PG_PASSWORD" psql "host=$PG_HOST user=$PG_USER dbname=postgres sslmode=require" -tc "SELECT 1 FROM pg_database WHERE datname='$PG_DATABASE'" | grep -q 1; then
        echo "Database '$PG_DATABASE' does not exist — creating it..."
        PGPASSWORD="$PG_PASSWORD" psql "host=$PG_HOST user=$PG_USER dbname=postgres sslmode=require" -c "CREATE DATABASE $PG_DATABASE;"
    fi

    echo "Database connectivity verified ($PG_HOST / $PG_DATABASE)"
    break
done

# Individual DB_* keys (what the API reads) plus a connection string
# for tools that prefer one.
PG_URL="postgresql://${PG_USER}:${PG_PASSWORD}@${PG_HOST}:5432/${PG_DATABASE}?sslmode=require"

create_sealed_secret \
    "postgresql-credentials" \
    "PostgreSQL / AWS RDS credentials" \
    "host=$PG_HOST" \
    "port=5432" \
    "username=$PG_USER" \
    "password=$PG_PASSWORD" \
    "database=$PG_DATABASE" \
    "connection-string=$PG_URL"

# Redis credentials. The in-cluster Redis (deploy/local/redis.yaml) has
# its password in the redis-secret — default to THAT value so the two
# can never drift apart (a mismatch shows up as NOAUTH at runtime).
echo ""
echo "Redis Credentials"
read -p "Enter Redis host [default: redis.rate-limit.svc.cluster.local]: " REDIS_HOST
REDIS_HOST=${REDIS_HOST:-redis.rate-limit.svc.cluster.local}

DEPLOYED_REDIS_PW=$(kubectl get secret redis-secret -n rate-limit -o jsonpath='{.data.redis-password}' 2>/dev/null | base64 -d || true)
if [ -n "$DEPLOYED_REDIS_PW" ]; then
    read -sp "Enter Redis password [default: use the deployed redis-secret]: " REDIS_PASSWORD
    echo ""
    REDIS_PASSWORD=${REDIS_PASSWORD:-$DEPLOYED_REDIS_PW}
else
    read -sp "Enter Redis password: " REDIS_PASSWORD
    echo ""
fi

REDIS_URL="redis://${REDIS_HOST}:6379"

create_sealed_secret \
    "redis-credentials" \
    "Redis connection URL and password" \
    "url=$REDIS_URL" \
    "password=$REDIS_PASSWORD"

# Harbor credentials
echo ""
echo "Harbor Registry Credentials"
read -p "Enter Harbor URL [default: https://registry.teepin.io]: " HARBOR_URL
HARBOR_URL=${HARBOR_URL:-https://registry.teepin.io}
read -p "Enter Harbor admin username [default: admin]: " HARBOR_USER
HARBOR_USER=${HARBOR_USER:-admin}
read -sp "Enter Harbor admin password: " HARBOR_PASSWORD
echo ""

create_sealed_secret \
    "harbor-credentials" \
    "Harbor container registry credentials" \
    "url=$HARBOR_URL" \
    "username=$HARBOR_USER" \
    "password=$HARBOR_PASSWORD"

# JWT Secret for API authentication
echo ""
echo "JWT Secret (or press Enter to generate)"
read -sp "Enter JWT secret (or leave empty to auto-generate): " JWT_SECRET
echo ""
if [ -z "$JWT_SECRET" ]; then
    JWT_SECRET=$(openssl rand -base64 32)
    echo "  Generated JWT secret: $JWT_SECRET"
fi

create_sealed_secret \
    "jwt-secret" \
    "JWT signing secret for API authentication" \
    "secret=$JWT_SECRET"

# Encryption key for billing data
echo ""
echo "Encryption Key (or press Enter to generate)"
read -sp "Enter AES encryption key (or leave empty to auto-generate): " ENCRYPT_KEY
echo ""
if [ -z "$ENCRYPT_KEY" ]; then
    ENCRYPT_KEY=$(openssl rand -base64 32)
    echo "  Generated encryption key: $ENCRYPT_KEY"
fi

create_sealed_secret \
    "encryption-key" \
    "AES-256 encryption key for sensitive data" \
    "key=$ENCRYPT_KEY"

# Admin API token (pricing management, /v1/admin). Auto-generated;
# operators read it back from the cluster when needed.
echo ""
echo "Admin API Token (or press Enter to generate)"
read -sp "Enter admin API token (or leave empty to auto-generate): " ADMIN_TOKEN
echo ""
if [ -z "$ADMIN_TOKEN" ]; then
    ADMIN_TOKEN=$(openssl rand -hex 32)
    echo "  Generated admin API token: $ADMIN_TOKEN"
    echo "  (SAVE THIS — it authenticates GET/PUT /v1/admin/pricing)"
fi

create_sealed_secret \
    "admin-api-token" \
    "Operator token for the /v1/admin API (pricing management)" \
    "token=$ADMIN_TOKEN"

# Cloudflare API token (for ExternalDNS)
echo ""
echo "Cloudflare Credentials (for DNS automation)"
read -p "Enter Cloudflare API token: " CF_TOKEN
read -p "Enter Cloudflare Zone ID: " CF_ZONE_ID

create_sealed_secret \
    "cloudflare-credentials" \
    "Cloudflare API token and zone for ExternalDNS" \
    "api-token=$CF_TOKEN" \
    "zone-id=$CF_ZONE_ID"

echo ""
echo "All sealed secrets created in: $OUTPUT_DIR/"
echo ""
echo "IMPORTANT:"
echo "  1. These sealed secrets are SAFE to commit to Git"
echo "  2. Only the Kubernetes cluster can decrypt them"
echo "  3. Keep the original passwords in a password manager"
echo "  4. Never commit the plain-text secrets!"
echo ""
echo "Next steps:"
echo "  git add $OUTPUT_DIR/"
echo "  git commit -m 'feat(security): add production sealed secrets'"
echo "  git push"
echo ""
