#!/bin/bash
set -e


# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi
echo "🔄 Applying database migrations..."

# Get PostgreSQL pod name
POSTGRES_POD=$(kubectl get pod -n teepin -l app=postgres -o jsonpath='{.items[0].metadata.name}')
echo "PostgreSQL pod: $POSTGRES_POD"

# Copy migration files to pod
echo "📁 Copying migration files to pod..."
kubectl cp migrations $POSTGRES_POD:/tmp/migrations -n teepin

# Run migrations
echo ""
echo "=== Running Migration 001: Auth Schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -f /tmp/migrations/001_create_auth_schema.up.sql

echo ""
echo "=== Running Migration 002: Compute Schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -f /tmp/migrations/002_create_compute_schema.up.sql

echo ""
echo "=== Running Migration 003: Billing Schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -f /tmp/migrations/003_create_billing_schema.up.sql

echo ""
echo "✅ All migrations completed successfully!"

# Verify
echo ""
echo "=== Verifying Schemas ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -c "SELECT schema_name FROM information_schema.schemata WHERE schema_name IN ('auth', 'compute', 'billing');"

echo ""
echo "=== Auth Tables ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -c "SELECT table_name FROM information_schema.tables WHERE table_schema = 'auth' ORDER BY table_name;"

echo ""
echo "=== Compute Tables ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -c "SELECT table_name FROM information_schema.tables WHERE table_schema = 'compute' ORDER BY table_name;"

echo ""
echo "=== Billing Tables ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -c "SELECT table_name FROM information_schema.tables WHERE table_schema = 'billing' ORDER BY table_name;"

echo ""
echo "=== Instance Types ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U teepin -d teepin_db -c "SELECT id, name, gpu_vram_gb, price_per_hour FROM compute.instance_types ORDER BY price_per_hour;"
