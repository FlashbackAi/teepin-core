#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi
#
# Production smoke test: exercises the full customer workflow against a
# live TEEPIN deployment and verifies billing records are written.
#
#   Register → login → project → API key →
#   deploy 20GB (exact-MIG path) → deploy 25GB (round-up path) →
#   list → get → logs → billing wired → delete → verify terminated
#
# Usage:
#   ./scripts/production-smoke-test.sh [API_URL]
#
#   API_URL default: http://localhost:8080 (use kubectl port-forward)
#   Set SKIP_GPU=true to run on a cluster without GPU capacity.
#   Set PSQL_POD=<pod> to also verify billing rows in PostgreSQL.

set -euo pipefail

API_URL="${1:-http://localhost:8080}"
SKIP_GPU="${SKIP_GPU:-false}"
STAMP=$(date +%s)
EMAIL="smoke-${STAMP}@test.teepin.io"
PASSWORD="Sm0ke-Test-$STAMP!"

PASS=0
FAIL=0
CREATED_IDS=()

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[1;33m'; NC='\033[0m'
ok()   { echo -e "${GREEN}✅ PASS${NC} $1"; PASS=$((PASS+1)); }
bad()  { echo -e "${RED}❌ FAIL${NC} $1"; FAIL=$((FAIL+1)); }
info() { echo -e "${YELLOW}▶${NC} $1"; }

need() { command -v "$1" > /dev/null || { echo "missing dependency: $1"; exit 1; }; }
need curl; need jq

cleanup() {
    for id in "${CREATED_IDS[@]:-}"; do
        [ -n "$id" ] && curl -s -X DELETE -H "Authorization: Bearer $API_KEY" \
            "$API_URL/v1/compute/instances/$id" > /dev/null || true
    done
}
trap cleanup EXIT

echo "==============================================="
echo " TEEPIN Production Smoke Test"
echo " API: $API_URL"
echo "==============================================="

# --- 0. Health ---------------------------------------------------------
info "Health check"
if curl -sf "$API_URL/health" | jq -e '.status == "healthy"' > /dev/null; then
    ok "API is healthy"
else
    bad "API health check failed — aborting"; exit 1
fi

# --- 1. Register + login ----------------------------------------------
info "Registering test user $EMAIL"
REG=$(curl -s -X POST "$API_URL/v1/auth/register" -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"name\":\"Smoke Test\"}")
if echo "$REG" | jq -e '.user.id // .id' > /dev/null 2>&1; then
    ok "User registered"
else
    bad "Registration failed: $REG"; exit 1
fi

TOKEN=$(curl -s -X POST "$API_URL/v1/auth/login" -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | jq -r '.access_token // empty')
[ -n "$TOKEN" ] && ok "Login returned JWT" || { bad "Login failed"; exit 1; }

# --- 2. Project + API key ----------------------------------------------
PROJECT_ID=$(curl -s -X POST "$API_URL/v1/projects" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"name":"smoke-test-project"}' | jq -r '.id // .project.id // empty')
[ -n "$PROJECT_ID" ] && ok "Project created: $PROJECT_ID" || { bad "Project creation failed"; exit 1; }

API_KEY=$(curl -s -X POST "$API_URL/v1/projects/$PROJECT_ID/api-keys" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"name":"smoke-test-key"}' | jq -r '.api_key // .key // empty')
[ -n "$API_KEY" ] && ok "API key issued" || { bad "API key creation failed"; exit 1; }

AUTH=(-H "Authorization: Bearer $API_KEY")

# --- 3. Tenancy: unauthenticated access must be rejected ---------------
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/v1/compute/instances")
[ "$CODE" = "401" ] && ok "Unauthenticated list rejected (401)" || bad "Expected 401 for unauthenticated list, got $CODE"

# --- 4. Instance types reflect real hardware ---------------------------
info "Discovering instance types"
TYPES=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instance-types")
COUNT=$(echo "$TYPES" | jq '.instance_types | length')
if [ "$COUNT" -gt 0 ]; then
    ok "Discovered $COUNT instance types from hardware"
    echo "$TYPES" | jq -r '.instance_types[] | "     \(.name)  \(.gpu_vram)  $\(.price_per_hour)/hr"'
else
    bad "No instance types discovered — is the GPU Operator publishing labels?"
fi

if [ "$SKIP_GPU" = "true" ]; then
    info "SKIP_GPU=true — skipping GPU instance tests"
else
    # --- 5. Exact-MIG path: 20GB --------------------------------------
    info "Creating 20GB instance (exact MIG profile path)"
    R20=$(curl -s -X POST "${AUTH[@]}" -H "Content-Type: application/json" \
        "$API_URL/v1/compute/instances" \
        -d '{"name":"smoke-mig20","image":"nvidia/cuda:12.3.1-base-ubuntu22.04","gpu_vram":"20GB","cpu_units":2,"memory":"8GB","env":{"SLEEP":"1"}}')
    ID20=$(echo "$R20" | jq -r '.id // empty')
    if [ -n "$ID20" ]; then
        CREATED_IDS+=("$ID20")
        ok "20GB instance created: $ID20 (type: $(echo "$R20" | jq -r .instance_type), \$$(echo "$R20" | jq -r .price_per_hour)/hr)"
        [ "$(echo "$R20" | jq -r .allocated_vram)" = "20GB" ] \
            && ok "20GB allocated exactly (MIG)" \
            || bad "Expected 20GB allocation, got $(echo "$R20" | jq -r .allocated_vram)"
    else
        bad "20GB instance creation failed: $R20"
    fi

    # --- 6. Round-up path: 25GB ----------------------------------------
    info "Creating 25GB instance (round-up path)"
    R25=$(curl -s -X POST "${AUTH[@]}" -H "Content-Type: application/json" \
        "$API_URL/v1/compute/instances" \
        -d '{"name":"smoke-roundup25","image":"nvidia/cuda:12.3.1-base-ubuntu22.04","gpu_vram":"25GB","cpu_units":2,"memory":"8GB"}')
    ID25=$(echo "$R25" | jq -r '.id // empty')
    if [ -n "$ID25" ]; then
        CREATED_IDS+=("$ID25")
        ALLOC=$(echo "$R25" | jq -r .allocated_vram)
        NOTE=$(echo "$R25" | jq -r '.allocation_note // empty')
        ok "25GB request allocated as $ALLOC: $ID25"
        [ -n "$NOTE" ] && ok "Transparent allocation note present: \"$NOTE\"" \
                       || bad "Missing allocation_note for rounded-up request"
    else
        bad "25GB instance creation failed: $R25"
    fi

    # --- 7. Wait for scheduling, then verify status --------------------
    info "Waiting up to 120s for instances to run..."
    for i in $(seq 1 24); do
        STATUS=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances/$ID20" | jq -r '.status // empty')
        [ "$STATUS" = "Running" ] && break
        sleep 5
    done
    [ "$STATUS" = "Running" ] && ok "20GB instance is Running on real GPU" \
                              || bad "20GB instance status after 120s: $STATUS (check: kubectl describe pod)"

    # --- 8. Logs --------------------------------------------------------
    LOGS=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances/$ID20/logs?tail=10")
    echo "$LOGS" | jq -e 'has("logs")' > /dev/null && ok "Log retrieval works" || bad "Log retrieval failed: $LOGS"

    # --- 9. List scoped to project --------------------------------------
    LIST_COUNT=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances" | jq '.count')
    [ "$LIST_COUNT" -ge 2 ] && ok "List shows this project's instances ($LIST_COUNT)" || bad "List count unexpected: $LIST_COUNT"

    # --- 10. Delete + verify -------------------------------------------
    info "Deleting instances"
    for id in "$ID20" "$ID25"; do
        [ -z "$id" ] && continue
        CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "${AUTH[@]}" "$API_URL/v1/compute/instances/$id")
        [ "$CODE" = "200" ] && ok "Deleted $id" || bad "Delete $id returned $CODE"
    done
    CREATED_IDS=()

    CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "${AUTH[@]}" "$API_URL/v1/compute/instances/inst-nonexist")
    [ "$CODE" = "404" ] && ok "Deleting nonexistent instance returns 404" || bad "Expected 404, got $CODE"
fi

# --- 11. Billing verification (optional, needs cluster access) ---------
if [ -n "${PSQL_POD:-}" ]; then
    info "Verifying instance rows in PostgreSQL ($PSQL_POD)"
    ROWS=$(kubectl exec "$PSQL_POD" -- psql -U teepin -d teepin_db -tAc \
        "SELECT count(*) FROM compute.instances WHERE project_id = '$PROJECT_ID'" 2>/dev/null || echo 0)
    [ "${ROWS:-0}" -ge 2 ] && ok "Instances persisted to database ($ROWS rows)" || bad "Expected >=2 DB rows, got $ROWS"

    TERM=$(kubectl exec "$PSQL_POD" -- psql -U teepin -d teepin_db -tAc \
        "SELECT count(*) FROM compute.instances WHERE project_id = '$PROJECT_ID' AND terminated_at IS NOT NULL" 2>/dev/null || echo 0)
    [ "${TERM:-0}" -ge 2 ] && ok "Deleted instances have terminated_at stamped ($TERM)" || bad "terminated_at missing (got $TERM)"
else
    info "PSQL_POD not set — skipping direct database verification"
fi

# --- Summary ------------------------------------------------------------
echo
echo "==============================================="
echo -e " Results: ${GREEN}$PASS passed${NC}, ${RED}$FAIL failed${NC}"
echo "==============================================="
[ "$FAIL" -eq 0 ]
