#!/bin/bash
# Copyright 2026 TEEPIN Project
# Licensed under the Apache License, Version 2.0

# Regenerates the Go bindings for proto/agent/v1/agent.proto.
#
# Run this after ANY change to the .proto. The generated files are
# committed, so a contributor does not need protoc to build the project —
# only to change the contract.
#
# Downloads protoc into .tools/ if absent rather than requiring a system
# install: the version must match across machines or the generated code
# drifts between contributors for no functional reason.
#
# Usage:
#   bash scripts/generate-proto.sh

if [ -z "${BASH_VERSION:-}" ]; then
    exec bash "$0" "$@"
fi

set -euo pipefail

PROTOC_VERSION="25.1"
PROTOC_GEN_GO_VERSION="v1.31.0"
PROTOC_GEN_GO_GRPC_VERSION="v1.3.0"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TOOLS_DIR="$REPO_ROOT/.tools"

MODULE="github.com/FlashbackAi/teepin-core"

info() { echo "[INFO] $1"; }
fail() { echo "[ERROR] $1" >&2; exit 1; }

command -v go > /dev/null || fail "go not found"

# Plugin binaries land in GOPATH/bin and must be on PATH for protoc to
# find them — protoc locates plugins by name, not by flag.
GOBIN="$(go env GOPATH)/bin"
export PATH="$PATH:$GOBIN"

if ! command -v protoc-gen-go > /dev/null; then
    info "Installing protoc-gen-go $PROTOC_GEN_GO_VERSION..."
    go install "google.golang.org/protobuf/cmd/protoc-gen-go@$PROTOC_GEN_GO_VERSION"
fi

if ! command -v protoc-gen-go-grpc > /dev/null; then
    info "Installing protoc-gen-go-grpc $PROTOC_GEN_GO_GRPC_VERSION..."
    go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@$PROTOC_GEN_GO_GRPC_VERSION"
fi

# protoc itself is a C++ binary with no `go install` path, so it is
# fetched into .tools/ (gitignored) and pinned to PROTOC_VERSION.
PROTOC="$TOOLS_DIR/protoc/bin/protoc"
if [ ! -x "$PROTOC" ] && [ ! -x "$PROTOC.exe" ]; then
    info "Downloading protoc $PROTOC_VERSION..."
    mkdir -p "$TOOLS_DIR/protoc"

    case "$(uname -s)" in
        Linux*)  ARCHIVE="protoc-${PROTOC_VERSION}-linux-x86_64.zip" ;;
        Darwin*) ARCHIVE="protoc-${PROTOC_VERSION}-osx-universal_binary.zip" ;;
        MINGW*|MSYS*|CYGWIN*) ARCHIVE="protoc-${PROTOC_VERSION}-win64.zip" ;;
        *) fail "Unsupported platform $(uname -s) — install protoc manually" ;;
    esac

    URL="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/${ARCHIVE}"
    TMP_ZIP="$TOOLS_DIR/protoc.zip"

    if command -v curl > /dev/null; then
        curl -fsSL "$URL" -o "$TMP_ZIP"
    elif command -v wget > /dev/null; then
        wget -q "$URL" -O "$TMP_ZIP"
    else
        fail "neither curl nor wget available to download protoc"
    fi

    if command -v unzip > /dev/null; then
        unzip -oq "$TMP_ZIP" -d "$TOOLS_DIR/protoc"
    else
        # Git bash on Windows often lacks unzip; PowerShell always has this.
        powershell -NoProfile -Command \
            "Expand-Archive -Path '$(cygpath -w "$TMP_ZIP" 2>/dev/null || echo "$TMP_ZIP")' -DestinationPath '$(cygpath -w "$TOOLS_DIR/protoc" 2>/dev/null || echo "$TOOLS_DIR/protoc")' -Force"
    fi
    rm -f "$TMP_ZIP"
fi

[ -x "$PROTOC" ] || PROTOC="$PROTOC.exe"
[ -x "$PROTOC" ] || fail "protoc still not executable at $PROTOC"

info "protoc: $("$PROTOC" --version)"

cd "$REPO_ROOT"
mkdir -p pkg/agentpb

# --*_opt=module strips the go_package prefix so files land in
# pkg/agentpb/ rather than a nested github.com/... tree.
info "Generating Go bindings..."
"$PROTOC" \
    --proto_path=proto \
    --go_out=. --go_opt="module=$MODULE" \
    --go-grpc_out=. --go-grpc_opt="module=$MODULE" \
    proto/agent/v1/agent.proto

info "Generated:"
ls -1 pkg/agentpb/

go build ./pkg/agentpb/ || fail "generated code does not compile"
info "Done."
