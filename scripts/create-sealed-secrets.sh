#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# Create Sealed Secrets for production deployment
# This encrypts sensitive data that can be safely committed to Git

set -e

NAMESPACE="${1:-teepin-prod}"
OUTPUT_DIR="deploy/production/secrets"

echo "🔒 Creating Sealed Secrets for namespace: $NAMESPACE"
echo ""

# Create output directory
mkdir -p "$OUTPUT_DIR"

# Function to create and seal a secret
create_sealed_secret() {
    local name=$1
    local key=$2
    local value=$3
    local description=$4

    echo "🔐 Creating sealed secret: $name"

    # Create regular secret (dry-run)
    kubectl create secret generic "$name" \
        --dry-run=client \
        --namespace="$NAMESPACE" \
        --from-literal="$key"="$value" \
        -o yaml | \
    kubeseal \
        --controller-namespace=kube-system \
        --controller-name=sealed-secrets-controller \
        --format=yaml \
        > "$OUTPUT_DIR/$name.yaml"

    # Add description as comment
    sed -i "1i# $description" "$OUTPUT_DIR/$name.yaml"

    echo "  ✅ Created: $OUTPUT_DIR/$name.yaml"
}

# PostgreSQL / AWS RDS credentials
echo ""
echo "📝 PostgreSQL Credentials"
read -p "Enter PostgreSQL host (e.g., teepin-prod.xxxxxx.us-east-1.rds.amazonaws.com): " PG_HOST
read -p "Enter PostgreSQL username [default: teepin]: " PG_USER
PG_USER=${PG_USER:-teepin}
read -sp "Enter PostgreSQL password: " PG_PASSWORD
echo ""
read -p "Enter PostgreSQL database name [default: teepin]: " PG_DATABASE
PG_DATABASE=${PG_DATABASE:-teepin}

# Create PostgreSQL connection string
PG_URL="postgresql://${PG_USER}:${PG_PASSWORD}@${PG_HOST}:5432/${PG_DATABASE}?sslmode=require"

create_sealed_secret \
    "postgresql-credentials" \
    "connection-string" \
    "$PG_URL" \
    "PostgreSQL connection string for AWS RDS"

# Redis credentials
echo ""
echo "📝 Redis Credentials"
read -p "Enter Redis host [default: redis.rate-limit.svc.cluster.local]: " REDIS_HOST
REDIS_HOST=${REDIS_HOST:-redis.rate-limit.svc.cluster.local}
read -sp "Enter Redis password: " REDIS_PASSWORD
echo ""

REDIS_URL="redis://${REDIS_HOST}:6379"

create_sealed_secret \
    "redis-credentials" \
    "url" \
    "$REDIS_URL" \
    "Redis connection URL"

create_sealed_secret \
    "redis-credentials" \
    "password" \
    "$REDIS_PASSWORD" \
    "Redis authentication password"

# Harbor credentials
echo ""
echo "📝 Harbor Registry Credentials"
read -p "Enter Harbor URL [default: https://registry.teepin.io]: " HARBOR_URL
HARBOR_URL=${HARBOR_URL:-https://registry.teepin.io}
read -p "Enter Harbor admin username [default: admin]: " HARBOR_USER
HARBOR_USER=${HARBOR_USER:-admin}
read -sp "Enter Harbor admin password: " HARBOR_PASSWORD
echo ""

create_sealed_secret \
    "harbor-credentials" \
    "url" \
    "$HARBOR_URL" \
    "Harbor container registry URL"

create_sealed_secret \
    "harbor-credentials" \
    "username" \
    "$HARBOR_USER" \
    "Harbor admin username"

create_sealed_secret \
    "harbor-credentials" \
    "password" \
    "$HARBOR_PASSWORD" \
    "Harbor admin password"

# JWT Secret for API authentication
echo ""
echo "📝 JWT Secret (or press Enter to generate)"
read -sp "Enter JWT secret (or leave empty to auto-generate): " JWT_SECRET
echo ""
if [ -z "$JWT_SECRET" ]; then
    JWT_SECRET=$(openssl rand -base64 32)
    echo "  Generated JWT secret: $JWT_SECRET"
fi

create_sealed_secret \
    "jwt-secret" \
    "secret" \
    "$JWT_SECRET" \
    "JWT signing secret for API authentication"

# Encryption key for billing data
echo ""
echo "📝 Encryption Key (or press Enter to generate)"
read -sp "Enter AES encryption key (or leave empty to auto-generate): " ENCRYPT_KEY
echo ""
if [ -z "$ENCRYPT_KEY" ]; then
    ENCRYPT_KEY=$(openssl rand -base64 32)
    echo "  Generated encryption key: $ENCRYPT_KEY"
fi

create_sealed_secret \
    "encryption-key" \
    "key" \
    "$ENCRYPT_KEY" \
    "AES-256 encryption key for sensitive data"

# Cloudflare API token (for ExternalDNS)
echo ""
echo "📝 Cloudflare Credentials (for DNS automation)"
read -p "Enter Cloudflare API token: " CF_TOKEN
read -p "Enter Cloudflare Zone ID: " CF_ZONE_ID

create_sealed_secret \
    "cloudflare-credentials" \
    "api-token" \
    "$CF_TOKEN" \
    "Cloudflare API token for ExternalDNS"

create_sealed_secret \
    "cloudflare-credentials" \
    "zone-id" \
    "$CF_ZONE_ID" \
    "Cloudflare Zone ID"

echo ""
echo "✅ All sealed secrets created in: $OUTPUT_DIR/"
echo ""
echo "⚠️  IMPORTANT:"
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
