#!/bin/bash
set -e


# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi
echo "Running database migrations..."

# PostgreSQL connection details
POSTGRES_POD=$(kubectl get pod -n teepin -l app=postgres -o jsonpath='{.items[0].metadata.name}')
POSTGRES_USER="teepin"
POSTGRES_DB="teepin_db"

echo "PostgreSQL pod: $POSTGRES_POD"

# Function to run SQL file
run_migration() {
    local file=$1
    echo "Running migration: $file"
    kubectl exec -n teepin $POSTGRES_POD -- psql -U $POSTGRES_USER -d $POSTGRES_DB -c "$(cat $file)"
}

# Run migrations in order
echo ""
echo "=== Migration 001: Auth Schema ==="
run_migration "migrations/001_create_auth_schema.up.sql"

echo ""
echo "=== Migration 002: Compute Schema ==="
run_migration "migrations/002_create_compute_schema.up.sql"

echo ""
echo "=== Migration 003: Billing Schema ==="
run_migration "migrations/003_create_billing_schema.up.sql"

echo ""
echo "✅ All migrations completed successfully!"

# Verify schemas
echo ""
echo "=== Verifying schemas ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U $POSTGRES_USER -d $POSTGRES_DB -c "\dn"

echo ""
echo "=== Tables in auth schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U $POSTGRES_USER -d $POSTGRES_DB -c "\dt auth.*"

echo ""
echo "=== Tables in compute schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U $POSTGRES_USER -d $POSTGRES_DB -c "\dt compute.*"

echo ""
echo "=== Tables in billing schema ==="
kubectl exec -n teepin $POSTGRES_POD -- psql -U $POSTGRES_USER -d $POSTGRES_DB -c "\dt billing.*"
