#!/usr/bin/env bash
set -euo pipefail

command -v go >/dev/null 2>&1 || {
    echo "Go is not installed or not in PATH."
    echo
    case "$(uname -s)" in
        Darwin)  install_cmd="brew install go" ;;
        Linux)
            if command -v apt-get >/dev/null 2>&1; then
                install_cmd="sudo apt-get update && sudo apt-get install -y golang-go"
            elif command -v dnf >/dev/null 2>&1; then
                install_cmd="sudo dnf install -y golang"
            elif command -v pacman >/dev/null 2>&1; then
                install_cmd="sudo pacman -S --noconfirm go"
            elif command -v apk >/dev/null 2>&1; then
                install_cmd="apk add go"
            else
                echo "No supported package manager found. Install Go 1.25+ from https://go.dev/dl/"
                exit 1
            fi
            ;;
        *)  echo "Unsupported OS. Install Go 1.25+ from https://go.dev/dl/"
            exit 1
            ;;
    esac

    echo "Run:  ${install_cmd}"
    if [[ -r /dev/tty ]]; then
        printf "Run this now? [y/N] " > /dev/tty
        read -r answer < /dev/tty || answer=""
    elif [[ -t 0 ]]; then
        read -r -p "Run this now? [y/N] " answer
    else
        echo "Non-interactive mode. Run the command above manually."
        exit 1
    fi

    case "$answer" in
        [Yy]|[Yy][Ee][Ss])
            echo "Installing Go..."
            eval "$install_cmd" || { echo "FAILED: ${install_cmd}"; exit 1; }
            ;;
        *)
            echo "Run the command above and re-execute this script."
            exit 0
            ;;
    esac
}
REPO="Dauno/local-agent"
VERSION="${VERSION:-}"
DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
DEST_DIR="${PREFIX:-$HOME/.local-agent/bin}"
BIN="local-agent"

cleanup() {
    if [[ -n "${source_dir:-}" ]]; then
        rm -rf "$source_dir"
    fi
    if [[ -n "${build_dir:-}" ]]; then
        rm -rf "$build_dir"
    fi
}
trap cleanup EXIT

if [[ -z "$VERSION" ]] && [[ -f "go.mod" ]] && grep -q "github.com/Dauno/slack-local-agent" go.mod 2>/dev/null; then
    command -v git >/dev/null 2>&1 || { echo "ERROR: git is required when installing from a local checkout."; exit 1; }
    proj_dir="$(pwd)"
    VERSION="$(git -C "$proj_dir" describe --tags --exact-match HEAD 2>/dev/null || echo dev)"
else
    command -v curl >/dev/null 2>&1 || { echo "ERROR: curl is required to download a release."; exit 1; }
    command -v tar >/dev/null 2>&1 || { echo "ERROR: tar is required to extract a release."; exit 1; }

    if [[ -z "$VERSION" ]]; then
        VERSION="$(curl -fsSL --retry 3 "https://api.github.com/repos/${REPO}/releases/latest" | sed -n 's/^[[:space:]]*"tag_name": "\([^"]*\)".*/\1/p')"
        [[ -n "$VERSION" ]] || { echo "ERROR: Unable to resolve the latest release."; exit 1; }
    fi

    echo "Downloading ${REPO} ${VERSION}..."
    source_dir="$(mktemp -d)"
    curl -fsSL --retry 3 "https://api.github.com/repos/${REPO}/tarball/${VERSION}" | tar -xzf - -C "$source_dir"

    source_entries=("$source_dir"/*)
    if [[ ${#source_entries[@]} -ne 1 ]] || [[ ! -d "${source_entries[0]}" ]]; then
        echo "ERROR: Release archive has an unexpected layout."
        exit 1
    fi
    proj_dir="${source_entries[0]}"
    archive_commit="${proj_dir##*-}"
    if [[ ! "$archive_commit" =~ ^[0-9a-f]{7,40}$ ]]; then
        archive_commit="unknown"
    fi
fi

COMMIT="${COMMIT:-${archive_commit:-$(git -C "$proj_dir" rev-parse --short HEAD 2>/dev/null || echo unknown)}}"
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
