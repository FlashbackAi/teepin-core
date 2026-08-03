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

# RKE2 kubectl env (for the automatic port-forward below).
if ! command -v kubectl > /dev/null 2>&1; then
    export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
    export PATH=$PATH:/var/lib/rancher/rke2/bin
fi

# When targeting the default localhost URL with nothing listening,
# start the port-forward automatically (and clean it up on exit) —
# no separate manual `kubectl port-forward` step.
PF_PID=""
cleanup_pf() { [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true; }
trap cleanup_pf EXIT
if [ "$API_URL" = "http://localhost:8080" ] && \
   ! curl -s -m 2 "$API_URL/health" > /dev/null 2>&1 && \
   command -v kubectl > /dev/null 2>&1; then
    echo "Starting kubectl port-forward to svc/api-server (teepin-prod)..."
    kubectl -n teepin-prod port-forward svc/api-server 8080:80 > /dev/null 2>&1 &
    PF_PID=$!
    # Wait for the forward (and the API behind it) to answer, up to 20s.
    for i in $(seq 1 10); do
        curl -s -m 2 "$API_URL/health" > /dev/null 2>&1 && break
        sleep 2
    done
fi
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
PROJ_RESP=$(curl -s -X POST "$API_URL/v1/projects" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "{\"name\":\"smoke-project-$STAMP\"}")
PROJECT_ID=$(echo "$PROJ_RESP" | jq -r '.id // .project.id // empty')
[ -n "$PROJECT_ID" ] && ok "Project created: $PROJECT_ID" || { bad "Project creation failed: $PROJ_RESP"; exit 1; }

KEY_RESP=$(curl -s -X POST "$API_URL/v1/projects/$PROJECT_ID/api-keys" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d '{"name":"smoke-test-key"}')
# The secret is the "key" field (returned once); "api_key" is a metadata
# object — selecting it would inject a multi-line value into the auth
# header, which the HTTP server rejects with a plain-text 400.
API_KEY=$(echo "$KEY_RESP" | jq -r '[.key, .api_key] | map(select(type=="string")) | first // empty')
[ -n "$API_KEY" ] && ok "API key issued" || { bad "API key creation failed: $KEY_RESP"; exit 1; }

AUTH=(-H "Authorization: Bearer $API_KEY")

# --- 3. Tenancy: unauthenticated access must be rejected ---------------
CODE=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/v1/compute/instances")
[ "$CODE" = "401" ] && ok "Unauthenticated list rejected (401)" || bad "Expected 401 for unauthenticated list, got $CODE"

# --- 4. Instance types reflect real hardware ---------------------------
info "Discovering instance types"
TYPES=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instance-types" || echo "")
# Tolerate non-JSON responses (rollouts, proxies): never crash, always
# show what the API actually said.
COUNT=$(echo "$TYPES" | jq '.instance_types | length' 2>/dev/null || echo 0)
if [ "${COUNT:-0}" -gt 0 ] 2>/dev/null; then
    ok "Discovered $COUNT instance types from hardware"
    echo "$TYPES" | jq -r '.instance_types[] | "     \(.name)  \(.gpu_vram)  $\(.price_per_hour)/hr"'
else
    bad "No instance types discovered — response was: ${TYPES:-<empty>}"
fi

if [ "$SKIP_GPU" = "true" ]; then
    info "SKIP_GPU=true — skipping GPU instance tests"
else
    # Derive test sizes from the DISCOVERED hardware so the test works
    # on any GPU: A100 MIG profiles are 10/20/40GB, H100 10/20/40/80,
    # H200 18/35/71/141 — hardcoded sizes would falsely "round up".
    EXACT_GB=$(echo "$TYPES" | jq '[.instance_types[] | select(.description | contains("MIG")) | .gpu_memory_gb] | min // 0' 2>/dev/null || echo 0)
    if [ "${EXACT_GB:-0}" -gt 0 ] 2>/dev/null; then
        HAS_MIG=true
        ROUNDUP_GB=$((EXACT_GB + 5))
        info "Hardware-derived sizes: exact=${EXACT_GB}GB (smallest MIG profile), round-up=${ROUNDUP_GB}GB"
    else
        HAS_MIG=false
        EXACT_GB=$(echo "$TYPES" | jq '[.instance_types[].gpu_memory_gb] | min // 0' 2>/dev/null || echo 0)
        info "No MIG profiles exposed — whole-GPU mode: exact=${EXACT_GB}GB, round-up test skipped (one GPU, one instance)"
    fi

    # --- 5. Exact allocation path ---------------------------------------
    info "Creating ${EXACT_GB}GB instance (exact allocation path)"
    R20=$(curl -s -X POST "${AUTH[@]}" -H "Content-Type: application/json" \
        "$API_URL/v1/compute/instances" \
        -d "{\"name\":\"smoke-exact\",\"image\":\"nvidia/cuda:12.3.1-base-ubuntu22.04\",\"gpu_vram\":\"${EXACT_GB}GB\",\"cpu_units\":2,\"memory\":\"8GB\",\"env\":{\"SLEEP\":\"1\"}}")
    ID20=$(echo "$R20" | jq -r '.id // empty' 2>/dev/null || true)
    if [ -n "$ID20" ]; then
        CREATED_IDS+=("$ID20")
        ok "${EXACT_GB}GB instance created: $ID20 (type: $(echo "$R20" | jq -r .instance_type), \$$(echo "$R20" | jq -r .price_per_hour)/hr)"
        [ "$(echo "$R20" | jq -r .allocated_vram)" = "${EXACT_GB}GB" ] \
            && ok "${EXACT_GB}GB allocated exactly" \
            || bad "Expected ${EXACT_GB}GB allocation, got $(echo "$R20" | jq -r .allocated_vram)"
    else
        bad "${EXACT_GB}GB instance creation failed: $R20"
    fi

    # --- 6. Round-up path (MIG hardware only) ---------------------------
    ID25=""
    if [ "$HAS_MIG" = "true" ]; then
        info "Creating ${ROUNDUP_GB}GB instance (round-up path)"
        R25=$(curl -s -X POST "${AUTH[@]}" -H "Content-Type: application/json" \
            "$API_URL/v1/compute/instances" \
            -d "{\"name\":\"smoke-roundup\",\"image\":\"nvidia/cuda:12.3.1-base-ubuntu22.04\",\"gpu_vram\":\"${ROUNDUP_GB}GB\",\"cpu_units\":2,\"memory\":\"8GB\"}")
        ID25=$(echo "$R25" | jq -r '.id // empty' 2>/dev/null || true)
        if [ -n "$ID25" ]; then
            CREATED_IDS+=("$ID25")
            ALLOC=$(echo "$R25" | jq -r .allocated_vram)
            NOTE=$(echo "$R25" | jq -r '.allocation_note // empty')
            ok "${ROUNDUP_GB}GB request allocated as $ALLOC: $ID25"
            [ -n "$NOTE" ] && ok "Transparent allocation note present: \"$NOTE\"" \
                           || bad "Missing allocation_note for rounded-up request"
        else
            bad "${ROUNDUP_GB}GB instance creation failed: $R25"
        fi
    fi

    # --- 7. Wait for scheduling, then verify status --------------------
    info "Waiting up to 120s for instances to run..."
    for i in $(seq 1 24); do
        STATUS=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances/$ID20" | jq -r '.status // empty' 2>/dev/null || true)
        [ "$STATUS" = "Running" ] && break
        sleep 5
    done
    [ "$STATUS" = "Running" ] && ok "${EXACT_GB}GB instance is Running on real GPU" \
                              || bad "${EXACT_GB}GB instance status after 120s: $STATUS (check: kubectl describe pod)"

    # --- 8. Logs --------------------------------------------------------
    LOGS=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances/$ID20/logs?tail=10")
    echo "$LOGS" | jq -e 'has("logs")' > /dev/null && ok "Log retrieval works" || bad "Log retrieval failed: $LOGS"

    # --- 9. List scoped to project --------------------------------------
    EXPECT_COUNT=1
    [ "$HAS_MIG" = "true" ] && EXPECT_COUNT=2
    LIST_COUNT=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances" | jq '.count' 2>/dev/null || echo 0)
    [ "$LIST_COUNT" -ge "$EXPECT_COUNT" ] && ok "List shows this project's instances ($LIST_COUNT)" || bad "List count unexpected: $LIST_COUNT"

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
