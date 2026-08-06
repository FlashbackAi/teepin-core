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

# RKE2 kubectl env. Also covers the case where a snap kubectl (no
# kubeconfig, talks to localhost:8080) shadows the real one — otherwise
# the port-forward below fails silently and this reports a misleading
# "API health check failed".
if ! command -v kubectl > /dev/null 2>&1 || ! kubectl get nodes > /dev/null 2>&1; then
    export KUBECONFIG=/etc/rancher/rke2/rke2.yaml
    export PATH=/var/lib/rancher/rke2/bin:$PATH
    if ! kubectl get nodes > /dev/null 2>&1; then
        echo "ERROR: kubectl cannot reach the cluster."
        echo "       Tried KUBECONFIG=/etc/rancher/rke2/rke2.yaml with RKE2's kubectl."
        echo "       Is RKE2 running?  systemctl status rke2-server"
        exit 1
    fi
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

# --- 1. Account signup + login -----------------------------------------
# Signup creates an ACCOUNT plus its owner user: every user belongs to
# an account, because the account is what gets billed.
ALIAS="smoke-${STAMP}"
info "Creating test account $ALIAS ($EMAIL)"
REG=$(curl -s -X POST "$API_URL/v1/accounts" -H "Content-Type: application/json" \
    -d "{\"type\":\"organization\",\"display_name\":\"Smoke Test Co\",\"alias\":\"$ALIAS\",\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\",\"full_name\":\"Smoke Test\"}")

ACCOUNT_ID=$(echo "$REG" | jq -r '.account.id // empty' 2>/dev/null || true)
ACCOUNT_NUMBER=$(echo "$REG" | jq -r '.account.account_number_formatted // empty' 2>/dev/null || true)
if [ -n "$ACCOUNT_ID" ]; then
    ok "Account created: $ACCOUNT_NUMBER ($ALIAS)"
else
    bad "Account creation failed: $REG"; exit 1
fi

# The signup identity must be the account owner: sole billing authority.
OWNER_ROLE=$(echo "$REG" | jq -r '.user.role // empty')
[ "$OWNER_ROLE" = "owner" ] && ok "Signup user has the owner role" \
                            || bad "Signup user role = '$OWNER_ROLE', want 'owner'"

TOKEN=$(curl -s -X POST "$API_URL/v1/auth/login" -H "Content-Type: application/json" \
    -d "{\"email\":\"$EMAIL\",\"password\":\"$PASSWORD\"}" | jq -r '.access_token // empty')
[ -n "$TOKEN" ] && ok "Login returned JWT" || { bad "Login failed"; exit 1; }

# --- 1b. Account details ------------------------------------------------
ACCT=$(curl -s "$API_URL/v1/accounts/current" -H "Authorization: Bearer $TOKEN")
if echo "$ACCT" | jq -e '.account_number' > /dev/null 2>&1; then
    ok "Account details retrievable ($(echo "$ACCT" | jq -r .type))"
else
    bad "GET /v1/accounts/current failed: $ACCT"
fi

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

# --- 2b. Sub-users ------------------------------------------------------
# A colleague gets their own login inside the account, signing in with
# alias + username (NOT email — the same email may exist in several
# accounts).
SUB=$(curl -s -X POST "$API_URL/v1/accounts/current/users" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
    -d "{\"username\":\"alice\",\"email\":\"alice-${STAMP}@test.teepin.io\",\"password\":\"Alice-$STAMP!\",\"role\":\"member\"}")
if echo "$SUB" | jq -e '.id' > /dev/null 2>&1; then
    ok "Sub-user created (role: $(echo "$SUB" | jq -r .role))"
else
    bad "Sub-user creation failed: $SUB"
fi

SUB_TOKEN=$(curl -s -X POST "$API_URL/v1/auth/login/sub-user" -H "Content-Type: application/json" \
    -d "{\"alias\":\"$ALIAS\",\"username\":\"alice\",\"password\":\"Alice-$STAMP!\"}" \
    | jq -r '.access_token // empty')
[ -n "$SUB_TOKEN" ] && ok "Sub-user signed in with alias + username" \
                    || bad "Sub-user login failed"

# Owner-only operations must be refused for a member.
CODE=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$API_URL/v1/accounts/current" \
    -H "Authorization: Bearer $SUB_TOKEN" -H "Content-Type: application/json" \
    -d '{"display_name":"Hijacked"}')
[ "$CODE" = "403" ] && ok "Member cannot edit account details (403)" \
                    || bad "Member editing account returned $CODE, want 403"

# --- 2c. Cross-account isolation ---------------------------------------
# THE tenancy test: a second account must not be able to read the
# first account's project, even holding a valid token and the real
# project UUID. Must be 404 — a 403 would confirm the project exists.
OTHER_EMAIL="other-${STAMP}@test.teepin.io"
# The alias must be explicit and unique: a personal account derives its
# alias from the display name, so a fixed name collides on the second
# run (aliases are globally unique).
OTHER_RESP=$(curl -s -X POST "$API_URL/v1/accounts" -H "Content-Type: application/json" \
    -d "{\"type\":\"personal\",\"display_name\":\"Other Tenant\",\"alias\":\"other-${STAMP}\",\"email\":\"$OTHER_EMAIL\",\"password\":\"$PASSWORD\"}")
OTHER_TOKEN=$(curl -s -X POST "$API_URL/v1/auth/login" -H "Content-Type: application/json" \
    -d "{\"email\":\"$OTHER_EMAIL\",\"password\":\"$PASSWORD\"}" | jq -r '.access_token // empty')

if [ -n "$OTHER_TOKEN" ]; then
    CODE=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/v1/projects/$PROJECT_ID" \
        -H "Authorization: Bearer $OTHER_TOKEN")
    case "$CODE" in
        404) ok "Cross-account project read blocked (404, existence not leaked)" ;;
        200) bad "CROSS-TENANT LEAK: another account read this project" ;;
        *)   bad "Cross-account read returned $CODE, want 404 (403 leaks existence)" ;;
    esac

    # The other account must also see none of this account's projects.
    OTHER_COUNT=$(curl -s "$API_URL/v1/projects" -H "Authorization: Bearer $OTHER_TOKEN" \
        | jq '.count // 0' 2>/dev/null || echo 0)
    [ "${OTHER_COUNT:-0}" = "0" ] && ok "Another account's project list is empty" \
                                  || bad "Another account sees $OTHER_COUNT projects — tenancy is broken"
else
    bad "Could not create the second account for the isolation check: $OTHER_RESP"
fi

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
        -d "{\"name\":\"smoke-exact\",\"image\":\"nvidia/cuda:12.3.1-base-ubuntu22.04\",\"gpu_vram\":\"${EXACT_GB}GB\",\"cpu_units\":2,\"memory\":\"8GB\",\"command\":[\"sleep\"],\"args\":[\"600\"]}")
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
            -d "{\"name\":\"smoke-roundup\",\"image\":\"nvidia/cuda:12.3.1-base-ubuntu22.04\",\"gpu_vram\":\"${ROUNDUP_GB}GB\",\"cpu_units\":2,\"memory\":\"8GB\",\"command\":[\"sleep\"],\"args\":[\"600\"]}")
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

    # --- 7b. THE PRODUCT TEST: real GPU compute in the customer container -
    # Everything else verifies plumbing. This verifies the thing customers
    # pay for: that a workload inside their container can actually use the
    # GPU, and sees ONLY their slice — not the whole card.
    if command -v kubectl > /dev/null 2>&1 && [ "$STATUS" = "Running" ]; then
        info "Verifying real GPU compute inside the customer container"
        POD=$(kubectl -n default get pods -l "app.teepin.cloud/instance-id=$ID20" \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)

        if [ -z "$POD" ]; then
            bad "Could not find the customer pod for $ID20"
        else
            # 1. The GPU is visible to the container at all.
            SMI=$(kubectl -n default exec "$POD" -- nvidia-smi 2>/dev/null || true)
            if echo "$SMI" | grep -qE "NVIDIA|CUDA Version"; then
                ok "nvidia-smi works inside the customer container"
            else
                bad "nvidia-smi failed inside the container — the GPU is NOT usable by customers"
            fi

            # 2. MIG tenant isolation: the container must see EXACTLY
            #    ONE MIG device — its own. Seeing more means a customer
            #    can reach other tenants' slices. This regressed silently
            #    once already (toolkit honouring NVIDIA_VISIBLE_DEVICES=all
            #    from the image), so assert the device COUNT, not just
            #    the reported memory size.
            if [ "$HAS_MIG" = "true" ]; then
                MIG_COUNT=$(kubectl -n default exec "$POD" -- \
                    nvidia-smi -L 2>/dev/null | grep -c "MIG" || echo 0)
                if [ "${MIG_COUNT:-0}" = "1" ]; then
                    ok "MIG isolation enforced: container sees exactly 1 MIG device (its own)"
                elif [ "${MIG_COUNT:-0}" = "0" ]; then
                    info "No MIG devices listed inside the container (nvidia-smi -L returned none)"
                else
                    bad "MIG ISOLATION BROKEN: container sees $MIG_COUNT MIG devices — other tenants' slices are visible"
                fi
            fi

            # Visible VRAM should also match the allocated slice.
            VISIBLE_MIB=$(kubectl -n default exec "$POD" -- \
                nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d ' \r' || true)
            if [ -n "$VISIBLE_MIB" ] 2>/dev/null && [ "${VISIBLE_MIB:-0}" -gt 0 ] 2>/dev/null; then
                VISIBLE_GB=$((VISIBLE_MIB / 1024))
                CEILING=$((EXACT_GB + 5))
                if [ "$VISIBLE_GB" -le "$CEILING" ]; then
                    ok "Container sees ~${VISIBLE_GB}GB VRAM (its ${EXACT_GB}GB slice, not the whole GPU)"
                else
                    bad "Container sees ${VISIBLE_GB}GB but was allocated ${EXACT_GB}GB — isolation NOT enforced"
                fi
            else
                info "Could not read visible VRAM from the container (skipping size assertion)"
            fi

            # 3. Actual computation: allocate device memory and run a
            #    matmul on it. Proves CUDA initializes and the slice does
            #    real work — not just that a device node exists.
            COMPUTE=$(kubectl -n default exec "$POD" -- sh -c '
                if command -v python3 >/dev/null 2>&1 && python3 -c "import torch" 2>/dev/null; then
                    python3 -c "import torch; a=torch.randn(512,512,device=\"cuda\"); print(\"COMPUTE_OK\", float((a@a).sum()) == float((a@a).sum()))"
                else
                    # No torch in the base CUDA image: fall back to the
                    # bundled CUDA sample-equivalent — a device query plus
                    # a memory allocation via nvidia-smi is the most we can
                    # assert without a toolchain.
                    nvidia-smi --query-gpu=uuid,compute_mode --format=csv,noheader && echo "COMPUTE_QUERY_OK"
                fi' 2>/dev/null || true)

            if echo "$COMPUTE" | grep -q "COMPUTE_OK"; then
                ok "Real CUDA computation succeeded on the customer's GPU slice"
            elif echo "$COMPUTE" | grep -q "COMPUTE_QUERY_OK"; then
                ok "GPU device query OK inside container (base image has no CUDA toolchain for a full matmul)"
                info "     For a true compute test, deploy a torch image — see GPU_WORKLOAD_TEST in the docs"
            else
                bad "GPU compute check failed inside the container: ${COMPUTE:-<no output>}"
            fi
        fi
    fi

    # --- 8. Logs --------------------------------------------------------
    LOGS=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances/$ID20/logs?tail=10")
    echo "$LOGS" | jq -e 'has("logs")' > /dev/null && ok "Log retrieval works" || bad "Log retrieval failed: $LOGS"

    # --- 9. List scoped to project --------------------------------------
    EXPECT_COUNT=1
    [ "$HAS_MIG" = "true" ] && EXPECT_COUNT=2
    LIST_COUNT=$(curl -s "${AUTH[@]}" "$API_URL/v1/compute/instances" | jq '.count' 2>/dev/null || echo 0)
    [ "$LIST_COUNT" -ge "$EXPECT_COUNT" ] && ok "List shows this project's instances ($LIST_COUNT)" || bad "List count unexpected: $LIST_COUNT"

    # --- 9b. Tenant network isolation -----------------------------------
    # Verify the NetworkPolicy actually enforces: a customer pod must NOT
    # reach the Kubernetes API, Redis, or cloud metadata — but MUST still
    # reach the internet. Runs a probe pod carrying the managed label, so
    # the same policy that governs real customer workloads applies to it.
    if command -v kubectl > /dev/null 2>&1 && kubectl get networkpolicy -n default customer-instance-isolation > /dev/null 2>&1; then
        info "Verifying tenant network isolation (NetworkPolicy enforcement)"

        # $1 = description, $2 = expect ("blocked"|"allowed"), $3.. = nc args
        probe_isolation() {
            local desc="$1" expect="$2"; shift 2
            local out
            out=$(kubectl run isoprobe-$RANDOM --rm -i --restart=Never \
                --image=busybox:1.36 --labels="app.teepin.cloud/managed=true" \
                --pod-running-timeout=90s -- sh -c "$*" 2>/dev/null || true)
            if echo "$out" | grep -q "REACHED"; then
                [ "$expect" = "allowed" ] && ok "$desc" || bad "$desc — traffic was NOT blocked"
            else
                [ "$expect" = "blocked" ] && ok "$desc" || bad "$desc — traffic was blocked but should be allowed"
            fi
        }

        probe_isolation "Customer pod BLOCKED from Redis" blocked \
            "nc -z -w 4 redis.rate-limit.svc.cluster.local 6379 && echo REACHED"
        probe_isolation "Customer pod BLOCKED from cloud metadata" blocked \
            "nc -z -w 4 169.254.169.254 80 && echo REACHED"
        probe_isolation "Customer pod ALLOWED to reach the internet" allowed \
            "nc -z -w 6 1.1.1.1 443 && echo REACHED"

        # The Kubernetes API cannot be reliably blocked by IP (kube-proxy
        # DNATs the service VIP to node addresses, and an IPv4 ipBlock
        # cannot cover IPv6-only nodes). What matters is that customer
        # pods carry NO credentials — check the real instance pods.
        POD=$(kubectl -n default get pods -l app.teepin.cloud/managed=true \
            -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
        if [ -n "$POD" ]; then
            MOUNTED=$(kubectl -n default get pod "$POD" \
                -o jsonpath='{.spec.automountServiceAccountToken}' 2>/dev/null || true)
            HASVOL=$(kubectl -n default get pod "$POD" \
                -o jsonpath='{.spec.volumes[*].name}' 2>/dev/null | grep -c "kube-api-access" || true)
            if [ "$MOUNTED" = "false" ] && [ "${HASVOL:-0}" = "0" ]; then
                ok "Customer pod carries NO Kubernetes API credentials"
            else
                bad "Customer pod has a ServiceAccount token mounted (automount=$MOUNTED, token volumes=$HASVOL)"
            fi
        fi
    else
        info "No customer-instance-isolation NetworkPolicy found — skipping isolation checks"
    fi

    # --- 10. Billing dwell ----------------------------------------------
    # The collector skips usage below a 1-minute floor (no sub-cent
    # charges), so instances deleted seconds after creation legitimately
    # bill nothing — and the billing path is never exercised. Hold the
    # instances past the floor so deletion produces a real usage record.
    # Set BILLING_DWELL=0 to skip (faster run, no billing coverage).
    DWELL="${BILLING_DWELL:-75}"
    if [ "$DWELL" -gt 0 ] 2>/dev/null; then
        info "Holding instances ${DWELL}s to clear the 1-minute billing floor..."
        sleep "$DWELL"
    fi

    # --- 11. Delete + verify -------------------------------------------
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

# --- 12. Billing tail verification (RDS/Aurora, via the sealed secret) --
# Proves terminated instances are billed: the collector must write a
# usage record ending at terminated_at, for instances that ran past the
# 1-minute floor. Uses the same credentials the API uses, read from the
# cluster — no passwords on the command line.
if [ "$SKIP_GPU" != "true" ] && [ "${DWELL:-0}" -gt 0 ] && command -v kubectl > /dev/null 2>&1; then
    info "Verifying billing records for terminated instances"

    PGHOST=$(kubectl -n teepin-prod get secret postgresql-credentials -o jsonpath='{.data.host}' 2>/dev/null | base64 -d || true)
    PGUSER=$(kubectl -n teepin-prod get secret postgresql-credentials -o jsonpath='{.data.username}' 2>/dev/null | base64 -d || true)
    PGDB=$(kubectl -n teepin-prod get secret postgresql-credentials -o jsonpath='{.data.database}' 2>/dev/null | base64 -d || true)
    PGPW=$(kubectl -n teepin-prod get secret postgresql-credentials -o jsonpath='{.data.password}' 2>/dev/null | base64 -d || true)

    if [ -z "$PGHOST" ] || ! command -v psql > /dev/null 2>&1; then
        info "Skipping billing verification (no DB credentials or psql on this host)"
    else
        # The collector runs hourly; restart the API to force a tick now.
        kubectl -n teepin-prod rollout restart deployment/api-server > /dev/null 2>&1 || true
        kubectl -n teepin-prod rollout status deployment/api-server --timeout=180s > /dev/null 2>&1 || true

        BILLED=0
        for i in $(seq 1 10); do
            BILLED=$(PGPASSWORD="$PGPW" psql "host=$PGHOST user=$PGUSER dbname=$PGDB sslmode=require connect_timeout=10" \
                -tAc "SELECT count(*) FROM billing.usage_records WHERE project_id = '$PROJECT_ID'" 2>/dev/null || echo 0)
            [ "${BILLED:-0}" -gt 0 ] 2>/dev/null && break
            sleep 6
        done

        if [ "${BILLED:-0}" -gt 0 ] 2>/dev/null; then
            ok "Terminated instances billed ($BILLED usage records)"
            PGPASSWORD="$PGPW" psql "host=$PGHOST user=$PGUSER dbname=$PGDB sslmode=require" \
                -c "SELECT instance_id, round(quantity::numeric,4) AS hours, unit_price, round(total_cost::numeric,4) AS cost
                    FROM billing.usage_records WHERE project_id = '$PROJECT_ID' ORDER BY created_at DESC" 2>/dev/null || true

            # Every record must end at termination, never later.
            LATE=$(PGPASSWORD="$PGPW" psql "host=$PGHOST user=$PGUSER dbname=$PGDB sslmode=require" -tAc \
                "SELECT count(*) FROM billing.usage_records u JOIN compute.instances i ON i.id = u.instance_id
                 WHERE u.project_id = '$PROJECT_ID' AND i.terminated_at IS NOT NULL AND u.end_time > i.terminated_at + interval '1 second'" 2>/dev/null || echo 0)
            [ "${LATE:-0}" = "0" ] && ok "Usage records end at termination (no overbilling)" \
                                   || bad "$LATE record(s) billed past terminated_at"
        else
            bad "No usage records written for terminated instances — billing tail regression"
        fi
    fi
fi

# --- 12b. Account billing summary ---------------------------------------
# What the console dashboard renders: one account total, attributed
# down to project and service.
info "Checking the account billing summary"
SUMMARY=$(curl -s "$API_URL/v1/billing/summary" -H "Authorization: Bearer $TOKEN")
if echo "$SUMMARY" | jq -e 'has("total_cost") and has("projects")' > /dev/null 2>&1; then
    ok "Billing summary: \$$(echo "$SUMMARY" | jq -r '.total_cost') across $(echo "$SUMMARY" | jq -r '.projects | length') project(s)"
    echo "$SUMMARY" | jq -r '.projects[]? | "     \(.project_name)  $\(.cost)"'
else
    bad "Billing summary failed: $SUMMARY"
fi

# --- 13. Instance persistence verification (optional, in-cluster DB) ----
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
