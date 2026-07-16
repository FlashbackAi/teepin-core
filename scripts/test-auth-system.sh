#!/bin/bash
set -e


# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi
echo "🧪 Testing TEEPIN Authentication System"
echo "========================================"
echo ""

API_URL="http://localhost:8080"

# Colors for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Test counter
TESTS_PASSED=0
TESTS_FAILED=0

test_endpoint() {
    local name=$1
    local method=$2
    local endpoint=$3
    local data=$4
    local expected_status=$5
    local headers=$6

    echo -n "Testing: $name... "

    if [ -z "$headers" ]; then
        response=$(curl -s -w "\n%{http_code}" -X $method "$API_URL$endpoint" \
            -H "Content-Type: application/json" \
            -d "$data" 2>/dev/null || true)
    else
        response=$(curl -s -w "\n%{http_code}" -X $method "$API_URL$endpoint" \
            -H "Content-Type: application/json" \
            -H "$headers" \
            -d "$data" 2>/dev/null || true)
    fi

    status_code=$(echo "$response" | tail -n1)
    body=$(echo "$response" | sed '$d')

    if [ "$status_code" = "$expected_status" ]; then
        echo -e "${GREEN}✓ PASSED${NC} (HTTP $status_code)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        echo "$body" | jq '.' 2>/dev/null || echo "$body"
        echo "$body"
    else
        echo -e "${RED}✗ FAILED${NC} (Expected $expected_status, got $status_code)"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        echo "$body"
    fi
    echo ""
}

echo "=== Health Check ==="
test_endpoint "Health Check" "GET" "/health" "" "200"

echo "=== User Registration ==="
test_endpoint "Register User" "POST" "/v1/auth/register" \
    '{"email":"test@example.com","password":"password123","full_name":"Test User"}' \
    "201"

echo "=== User Login ==="
LOGIN_RESPONSE=$(curl -s -X POST "$API_URL/v1/auth/login" \
    -H "Content-Type: application/json" \
    -d '{"email":"test@example.com","password":"password123"}')

ACCESS_TOKEN=$(echo $LOGIN_RESPONSE | jq -r '.access_token')

if [ "$ACCESS_TOKEN" != "null" ] && [ -n "$ACCESS_TOKEN" ]; then
    echo -e "${GREEN}✓ Login successful${NC}"
    echo "Access token: ${ACCESS_TOKEN:0:20}..."
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Login failed${NC}"
    echo "$LOGIN_RESPONSE"
    TESTS_FAILED=$((TESTS_FAILED + 1))
fi
echo ""

echo "=== Get Current User ==="
test_endpoint "Get Current User" "GET" "/v1/auth/me" "" "200" "Authorization: Bearer $ACCESS_TOKEN"

echo "=== Create Project ==="
PROJECT_RESPONSE=$(curl -s -X POST "$API_URL/v1/projects" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $ACCESS_TOKEN" \
    -d '{"name":"My First Project","description":"Testing project creation"}')

PROJECT_ID=$(echo $PROJECT_RESPONSE | jq -r '.id')

if [ "$PROJECT_ID" != "null" ] && [ -n "$PROJECT_ID" ]; then
    echo -e "${GREEN}✓ Project created${NC}"
    echo "Project ID: $PROJECT_ID"
    echo "$PROJECT_RESPONSE" | jq '.'
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ Project creation failed${NC}"
    echo "$PROJECT_RESPONSE"
    TESTS_FAILED=$((TESTS_FAILED + 1))
fi
echo ""

echo "=== List Projects ==="
test_endpoint "List Projects" "GET" "/v1/projects" "" "200" "Authorization: Bearer $ACCESS_TOKEN"

echo "=== Create API Key ==="
APIKEY_RESPONSE=$(curl -s -X POST "$API_URL/v1/projects/$PROJECT_ID/api-keys" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $ACCESS_TOKEN" \
    -d '{"name":"My API Key","scopes":["instances:read","instances:write"]}')

API_KEY=$(echo $APIKEY_RESPONSE | jq -r '.key')

if [ "$API_KEY" != "null" ] && [ -n "$API_KEY" ]; then
    echo -e "${GREEN}✓ API Key created${NC}"
    echo "API Key: ${API_KEY:0:20}..."
    echo "$APIKEY_RESPONSE" | jq '.'
    TESTS_PASSED=$((TESTS_PASSED + 1))
else
    echo -e "${RED}✗ API Key creation failed${NC}"
    echo "$APIKEY_RESPONSE"
    TESTS_FAILED=$((TESTS_FAILED + 1))
fi
echo ""

echo "=== Test API Key Authentication ==="
test_endpoint "List Instance Types with API Key" "GET" "/v1/compute/instance-types" "" "200" "Authorization: Bearer $API_KEY"

echo "=== List API Keys ==="
test_endpoint "List API Keys" "GET" "/v1/projects/$PROJECT_ID/api-keys" "" "200" "Authorization: Bearer $ACCESS_TOKEN"

echo ""
echo "========================================"
echo "🎯 Test Summary"
echo "========================================"
echo -e "Tests Passed: ${GREEN}$TESTS_PASSED${NC}"
echo -e "Tests Failed: ${RED}$TESTS_FAILED${NC}"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}✅ All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}❌ Some tests failed${NC}"
    exit 1
fi
