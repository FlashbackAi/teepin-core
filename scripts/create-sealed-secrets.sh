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

NAMESPACE="${1:-teepin-prod}"
OUTPUT_DIR="deploy/production/secrets"

echo "🔒 Creating Sealed Secrets for namespace: $NAMESPACE"
echo ""

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

    echo "🔐 Creating sealed secret: $name ($# keys)"

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
    "Redis connection URL and password" \
    "url=$REDIS_URL" \
    "password=$REDIS_PASSWORD"

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
    "Harbor container registry credentials" \
    "url=$HARBOR_URL" \
    "username=$HARBOR_USER" \
    "password=$HARBOR_PASSWORD"

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
    "JWT signing secret for API authentication" \
    "secret=$JWT_SECRET"

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
    "AES-256 encryption key for sensitive data" \
    "key=$ENCRYPT_KEY"

# Cloudflare API token (for ExternalDNS)
echo ""
echo "📝 Cloudflare Credentials (for DNS automation)"
read -p "Enter Cloudflare API token: " CF_TOKEN
read -p "Enter Cloudflare Zone ID: " CF_ZONE_ID

create_sealed_secret \
    "cloudflare-credentials" \
    "Cloudflare API token and zone for ExternalDNS" \
    "api-token=$CF_TOKEN" \
    "zone-id=$CF_ZONE_ID"

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
