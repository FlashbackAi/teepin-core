#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# This is a bash script: re-exec with bash when invoked via `sh`.
if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

# Run comprehensive security audit on Kubernetes cluster
# Uses kube-bench for CIS Kubernetes Benchmark compliance

set -e

echo "🔒 TEEPIN Security Audit"
echo "========================"
echo ""

# Install kube-bench if not present
if ! command -v kube-bench &> /dev/null; then
    echo "📦 Installing kube-bench..."

    # Run kube-bench as a Kubernetes job (easier than local install)
    kubectl apply -f https://raw.githubusercontent.com/aquasecurity/kube-bench/main/job.yaml

    echo "⏳ Waiting for kube-bench job to complete..."
    kubectl wait --for=condition=complete --timeout=300s job/kube-bench -n default

    echo ""
    echo "📊 Kube-bench Results:"
    echo "===================="
    kubectl logs job/kube-bench -n default

    # Save results to file
    kubectl logs job/kube-bench -n default > kube-bench-results.txt
    echo ""
    echo "✅ Results saved to: kube-bench-results.txt"

    # Cleanup
    kubectl delete job kube-bench -n default
else
    # Run kube-bench locally
    echo "🔍 Running kube-bench..."
    kube-bench run --targets master,node,etcd,policies > kube-bench-results.txt

    echo ""
    cat kube-bench-results.txt
fi

echo ""
echo "🔍 Analyzing Results..."
echo ""

# Check for FAIL results
FAIL_COUNT=$(grep -c "\[FAIL\]" kube-bench-results.txt || true)
WARN_COUNT=$(grep -c "\[WARN\]" kube-bench-results.txt || true)
PASS_COUNT=$(grep -c "\[PASS\]" kube-bench-results.txt || true)

echo "Summary:"
echo "  ✅ PASS: $PASS_COUNT"
echo "  ⚠️  WARN: $WARN_COUNT"
echo "  ❌ FAIL: $FAIL_COUNT"
echo ""

if [ "$FAIL_COUNT" -gt 0 ]; then
    echo "⚠️  Critical issues found! Review kube-bench-results.txt"
    echo ""
    echo "Common fixes:"
    echo "  1. Enable RBAC: --authorization-mode=RBAC"
    echo "  2. Enable audit logging: --audit-log-path=/var/log/audit.log"
    echo "  3. Restrict anonymous auth: --anonymous-auth=false"
    echo "  4. Enable pod security policies"
    echo ""
fi

# Run additional security checks
echo "🔍 Additional Security Checks..."
echo ""

# Check 1: Privileged containers
echo "1. Checking for privileged containers..."
PRIVILEGED=$(kubectl get pods --all-namespaces -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].securityContext.privileged}{"\n"}{end}' | grep -c "true" || true)
if [ "$PRIVILEGED" -gt 0 ]; then
    echo "  ⚠️  Found $PRIVILEGED privileged containers"
else
    echo "  ✅ No privileged containers found"
fi

# Check 2: Containers running as root
echo "2. Checking for containers running as root..."
ROOT_CONTAINERS=$(kubectl get pods --all-namespaces -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[*].securityContext.runAsNonRoot}{"\n"}{end}' | grep -c "false" || true)
if [ "$ROOT_CONTAINERS" -gt 0 ]; then
    echo "  ⚠️  Found $ROOT_CONTAINERS containers running as root"
else
    echo "  ✅ All containers running as non-root"
fi

# Check 3: Default service account usage
echo "3. Checking for default service account usage..."
DEFAULT_SA=$(kubectl get pods --all-namespaces -o jsonpath='{range .items[*]}{.spec.serviceAccountName}{"\n"}{end}' | grep -c "^default$" || true)
if [ "$DEFAULT_SA" -gt 5 ]; then
    echo "  ⚠️  $DEFAULT_SA pods using default service account"
else
    echo "  ✅ Limited use of default service account"
fi

# Check 4: Secrets in environment variables
echo "4. Checking for secrets in environment variables..."
echo "  ℹ️  Manual review required (check for plain-text secrets)"

# Check 5: Network policies
echo "5. Checking network policies..."
NETPOL_COUNT=$(kubectl get networkpolicies --all-namespaces 2>/dev/null | wc -l)
if [ "$NETPOL_COUNT" -lt 2 ]; then
    echo "  ⚠️  No network policies found - consider implementing"
else
    echo "  ✅ Network policies in place"
fi

# Check 6: Pod Security Standards
echo "6. Checking Pod Security Standards..."
PSS_ENABLED=$(kubectl get ns --show-labels | grep -c "pod-security.kubernetes.io" || true)
if [ "$PSS_ENABLED" -eq 0 ]; then
    echo "  ⚠️  Pod Security Standards not configured"
    echo "     Consider adding to namespaces:"
    echo "     pod-security.kubernetes.io/enforce=restricted"
else
    echo "  ✅ Pod Security Standards configured"
fi

echo ""
echo "📊 Full Report Available: kube-bench-results.txt"
echo ""
echo "✅ Security audit complete!"
echo ""
echo "Next steps:"
echo "  1. Review critical FAIL items in kube-bench-results.txt"
echo "  2. Fix high-priority issues"
echo "  3. Re-run audit: ./scripts/run-security-audit.sh"
echo "  4. Document exceptions/risks"
echo ""
