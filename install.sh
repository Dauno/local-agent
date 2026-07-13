#!/usr/bin/env bash
set -euo pipefail

REPO_URL="${REPO_URL:-https://github.com/Dauno/local-agent.git}"
VERSION="${VERSION:-dev}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
DEST_DIR="${PREFIX:-$HOME/.local-agent/bin}"
BIN="local-agent"

cleanup() {
    if [[ -n "${clone_dir:-}" ]]; then
        rm -rf "$clone_dir"
    fi
    if [[ -n "${build_dir:-}" ]]; then
        rm -rf "$build_dir"
    fi
}
trap cleanup EXIT

if [[ -f "go.mod" ]] && grep -q "github.com/Dauno/slack-local-agent" go.mod 2>/dev/null; then
    proj_dir="$(pwd)"
else
    echo "Cloning ${REPO_URL}..."
    clone_dir="$(mktemp -d)"
    git clone --depth 1 "$REPO_URL" "$clone_dir"
    proj_dir="$clone_dir"
fi

COMMIT="${COMMIT:-$(git -C "$proj_dir" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
LDFLAGS="-s -w -X github.com/Dauno/slack-local-agent/internal/buildinfo.Version=${VERSION} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Commit=${COMMIT} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Date=${DATE}"

echo "Building ${BIN}..."
build_dir="$(mktemp -d)"
go build -C "$proj_dir" -trimpath -ldflags "${LDFLAGS}" -o "${build_dir}/${BIN}" ./cmd/local-agent

mkdir -p "${DEST_DIR}"
install -m 0755 "${build_dir}/${BIN}" "${DEST_DIR}/${BIN}"

echo "Installed ${DEST_DIR}/${BIN}"

if [[ ":$PATH:" != *":${DEST_DIR}:"* ]]; then
    echo
    echo "WARNING: ${DEST_DIR} is not in your PATH."
    echo "Add it with:  export PATH=\"\${PATH}:${DEST_DIR}\""
fi
