#!/usr/bin/env bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0
#
# Keeps a Kubernetes imagePullSecret current on a non-AWS TEEPIN node
# (home or datacenter) so its k3s can pull the Kumbha agent image from
# ECR. See ROADMAP.md's 2026-08-23 decision: ECR directly, not
# self-hosted Harbor — this script is the piece that decision leaves for
# the node itself to do, since the ECS-hosted control plane has no way
# to reach into this cluster and create anything here.
#
# WHY THIS EXISTS AT ALL: ECR authorization tokens expire every 12
# hours. A pull secret created once at pod-launch time would silently
# start failing image pulls a few hours later on any node whose pod got
# rescheduled or restarted — this refreshes it well inside that window
# instead of reacting to a failure.
#
# Reads AWS credentials from a local file rather than fetching them from
# Secrets Manager itself: fetching FROM Secrets Manager would need its
# own AWS credential already present (nothing gained), and it keeps the
# IAM user's permissions to exactly ECR pull (security.tf's
# kumbha_ecr_puller policy) — no secretsmanager:GetSecretValue grant
# needed just to bootstrap this script.
#
# Usage:
#   # One-time install: creates the credentials file (paste values when
#   # prompted... actually no prompting, see below), installs a systemd
#   # timer, and runs an immediate refresh.
#   sudo bash refresh-ecr-pull-secret.sh --install \
#       --account-id 880254196251 --region us-east-1 \
#       --secret-name teepin-kumbha-ecr --namespace default
#
#   # Then populate the credentials file this created (see the path it
#   # prints) with the access key from:
#   #   aws iam create-access-key --user-name teepin-kumbha-ecr-puller-dev
#
#   # Manual one-off refresh (also what the systemd timer calls):
#   sudo bash refresh-ecr-pull-secret.sh --once \
#       --account-id 880254196251 --region us-east-1 \
#       --secret-name teepin-kumbha-ecr --namespace default

set -euo pipefail

MODE=""
ACCOUNT_ID=""
REGION="us-east-1"
SECRET_NAME="teepin-kumbha-ecr"
NAMESPACE="default"
CREDS_FILE="/etc/teepin/kumbha-ecr-puller.env"
INTERVAL="6h"   # well under the 12h token expiry

while [ $# -gt 0 ]; do
    case "$1" in
        --install)      MODE="install"; shift ;;
        --once)         MODE="once"; shift ;;
        --account-id)   ACCOUNT_ID="$2"; shift 2 ;;
        --region)       REGION="$2"; shift 2 ;;
        --secret-name)  SECRET_NAME="$2"; shift 2 ;;
        --namespace)    NAMESPACE="$2"; shift 2 ;;
        --creds-file)   CREDS_FILE="$2"; shift 2 ;;
        --interval)     INTERVAL="$2"; shift 2 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

info() { echo "[ecr-refresh] $*"; }
fail() { echo "[ecr-refresh] ERROR: $*" >&2; exit 1; }

[ "$(id -u)" = "0" ] || fail "run as root (sudo)."
[ -n "$MODE" ] || fail "pass --install (first-time setup) or --once (a single refresh)."
[ -n "$ACCOUNT_ID" ] || fail "--account-id is required (the AWS account the kumbha-agent ECR repo lives in)."

# --- the actual refresh, used by both modes -------------------------------
do_refresh() {
    command -v aws >/dev/null 2>&1 || fail "AWS CLI not found. Install it first: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"

    KUBECTL="kubectl"
    if ! command -v kubectl >/dev/null 2>&1; then
        command -v k3s >/dev/null 2>&1 || fail "neither kubectl nor k3s found."
        KUBECTL="k3s kubectl"
    fi
    export KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

    [ -f "$CREDS_FILE" ] || fail "credentials file not found at $CREDS_FILE — create it first (see this script's usage comment): AWS_ACCESS_KEY_ID=... and AWS_SECRET_ACCESS_KEY=... on separate lines, mode 600."
    # shellcheck disable=SC1090
    set -a; source "$CREDS_FILE"; set +a
    [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
        || fail "$CREDS_FILE did not set both AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY."

    REGISTRY="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com"

    info "fetching a fresh ECR token for $REGISTRY..."
    TOKEN=$(AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
        aws ecr get-login-password --region "$REGION") \
        || fail "aws ecr get-login-password failed — check the IAM user's key is active and has ecr:GetAuthorizationToken."

    info "upserting Secret/$SECRET_NAME in namespace $NAMESPACE..."
    $KUBECTL create secret docker-registry "$SECRET_NAME" \
        --namespace "$NAMESPACE" \
        --docker-server="$REGISTRY" \
        --docker-username=AWS \
        --docker-password="$TOKEN" \
        --dry-run=client -o yaml | $KUBECTL apply -f - >/dev/null \
        || fail "kubectl apply failed."

    info "done. Secret/$SECRET_NAME in $NAMESPACE now authenticates against $REGISTRY."
}

if [ "$MODE" = "once" ]; then
    do_refresh
    exit 0
fi

# --- install mode: credentials file scaffold + systemd timer -------------
mkdir -p "$(dirname "$CREDS_FILE")"
if [ ! -f "$CREDS_FILE" ]; then
    cat > "$CREDS_FILE" <<'EOF'
# Populate from: aws iam create-access-key --user-name teepin-kumbha-ecr-puller-<env>
AWS_ACCESS_KEY_ID=
AWS_SECRET_ACCESS_KEY=
EOF
    chmod 600 "$CREDS_FILE"
    info "created $CREDS_FILE (empty) — fill in the two values before the timer's first real run."
else
    info "$CREDS_FILE already exists, leaving it as-is."
fi

SCRIPT_PATH="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"

cat > /etc/systemd/system/teepin-kumbha-ecr-refresh.service <<EOF
[Unit]
Description=Refresh the Kumbha agent image's ECR pull secret
After=network-online.target k3s.service
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/bin/env bash $SCRIPT_PATH --once --account-id $ACCOUNT_ID --region $REGION --secret-name $SECRET_NAME --namespace $NAMESPACE --creds-file $CREDS_FILE
EOF

cat > /etc/systemd/system/teepin-kumbha-ecr-refresh.timer <<EOF
[Unit]
Description=Periodically refresh the Kumbha agent image's ECR pull secret

[Timer]
OnBootSec=2min
OnUnitActiveSec=$INTERVAL
Persistent=true

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now teepin-kumbha-ecr-refresh.timer
info "timer installed, refreshing every $INTERVAL."
info "IMPORTANT: fill in $CREDS_FILE with the IAM user's access key, then run once by hand to confirm:"
info "  sudo systemctl start teepin-kumbha-ecr-refresh.service"
info "  sudo journalctl -u teepin-kumbha-ecr-refresh.service -n 50"
