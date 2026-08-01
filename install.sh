#!/usr/bin/env bash
set -euo pipefail

REPO="Dauno/local-agent"
BIN="local-agent"
MIN_GO_MAJOR=1
MIN_GO_MINOR=25
LOCAL_MODE=0
VERSION="${VERSION:-}"
COMMIT="${COMMIT:-}"
DEST_DIR="${PREFIX:-$HOME/.local-agent/bin}"

usage() {
    printf 'Usage: %s [--local]\n' "${0##*/}"
    printf '  --local  build the checkout in the current directory\n'
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --local)
            LOCAL_MODE=1
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            printf 'ERROR: Unknown argument: %s\n' "$1" >&2
            usage >&2
            exit 2
            ;;
    esac
done

temp_root=""
tmp_bin=""

cleanup() {
    local status=$?
    set +e
    if [[ -n "$tmp_bin" ]]; then
        rm -f -- "$tmp_bin"
    fi
    if [[ -n "$temp_root" ]]; then
        rm -rf -- "$temp_root"
    fi
    return "$status"
}
trap cleanup EXIT

required_commands=(curl tar uname grep sed date mktemp mkdir install rm mv)
for required_command in "${required_commands[@]}"; do
    if ! command -v "$required_command" >/dev/null 2>&1; then
        printf "ERROR: Required command '%s' was not found in PATH. Install it and re-run.\n" \
            "$required_command" >&2
        exit 1
    fi
done

if (( LOCAL_MODE )); then
    if ! command -v git >/dev/null 2>&1; then
        echo "ERROR: git is required when installing from a local checkout." >&2
        exit 1
    fi
else
    if ! command -v jq >/dev/null 2>&1; then
        echo "ERROR: jq is required to parse GitHub release metadata. Install jq or set VERSION to a release tag." >&2
        exit 1
    fi
    if command -v sha256sum >/dev/null 2>&1; then
        checksum_tool=sha256sum
    elif command -v shasum >/dev/null 2>&1; then
        checksum_tool=shasum
    else
        echo "ERROR: sha256sum or shasum is required to verify release downloads." >&2
        exit 1
    fi
fi

check_go_version() {
    local go_version major minor

    if ! command -v go >/dev/null 2>&1; then
        printf 'ERROR: Go %d.%d or later is required, but Go is not installed or not in PATH.\n' \
            "$MIN_GO_MAJOR" "$MIN_GO_MINOR" >&2
        return 1
    fi

    if ! go_version="$(go env GOVERSION 2>/dev/null)" || [[ -z "$go_version" ]]; then
        if ! go_version="$(go version 2>/dev/null | sed -n 's/^go version \([^ ]*\).*/\1/p')" || [[ -z "$go_version" ]]; then
            echo "ERROR: Unable to determine the installed Go version. Go 1.25 or later is required." >&2
            return 1
        fi
    fi

    if [[ ! "$go_version" =~ ^go([0-9]+)\.([0-9]+)(\.[0-9]+)?([[:alnum:].-]*)$ ]]; then
        printf 'ERROR: Unable to parse Go version %s. Go %d.%d or later is required.\n' \
            "$go_version" "$MIN_GO_MAJOR" "$MIN_GO_MINOR" >&2
        return 1
    fi
    major="${BASH_REMATCH[1]}"
    minor="${BASH_REMATCH[2]}"
    if (( major < MIN_GO_MAJOR || (major == MIN_GO_MAJOR && minor < MIN_GO_MINOR) )); then
        printf 'ERROR: Go %d.%d or later is required; found %s.\n' \
            "$MIN_GO_MAJOR" "$MIN_GO_MINOR" "$go_version" >&2
        return 1
    fi
}

install_go_prerequisite() {
    local os package_manager install_description answer
    local sudo_display=""
    local -a sudo_prefix=()

    os="$(uname -s)"
    case "$os" in
        Darwin)
            if ! command -v brew >/dev/null 2>&1; then
                echo "ERROR: Homebrew is required to install Go automatically on macOS." >&2
                echo "Install Go 1.25+ manually from https://go.dev/dl/, then re-run this script." >&2
                echo "Manual package command: brew install go" >&2
                return 1
            fi
            package_manager=brew
            install_description="brew install go"
            ;;
        Linux)
            if command -v apt-get >/dev/null 2>&1; then
                package_manager=apt-get
            elif command -v dnf >/dev/null 2>&1; then
                package_manager=dnf
            elif command -v pacman >/dev/null 2>&1; then
                package_manager=pacman
            elif command -v apk >/dev/null 2>&1; then
                package_manager=apk
            else
                echo "ERROR: No supported package manager found on Linux." >&2
                echo "Install Go 1.25+ manually from https://go.dev/dl/, then re-run this script." >&2
                return 1
            fi
            if (( EUID != 0 )); then
                if ! command -v sudo >/dev/null 2>&1; then
                    echo "ERROR: sudo is required to install Go when this script is not run as root." >&2
                    echo "Install Go 1.25+ manually from https://go.dev/dl/, then re-run this script." >&2
                    return 1
                fi
                sudo_prefix=(sudo)
                sudo_display="sudo "
            fi
            case "$package_manager" in
                apt-get)
                    install_description="${sudo_display}apt-get update && ${sudo_display}apt-get install -y golang-go"
                    ;;
                dnf)
                    install_description="${sudo_display}dnf install -y golang"
                    ;;
                pacman)
                    install_description="${sudo_display}pacman -S --noconfirm go"
                    ;;
                apk)
                    install_description="${sudo_display}apk add go"
                    ;;
            esac
            ;;
        *)
            printf 'ERROR: Unsupported OS: %s.\n' "$os" >&2
            echo "Install Go 1.25+ manually from https://go.dev/dl/, then re-run this script." >&2
            return 1
            ;;
    esac

    echo "Go is required to build ${BIN}."
    echo "Run:  ${install_description}"
    if { exec 3<>/dev/tty; } 2>/dev/null; then
        printf 'Run this now? [y/N] ' >&3
        if ! IFS= read -r answer <&3; then
            answer=""
        fi
        exec 3>&-
    elif [[ -t 0 ]]; then
        if ! IFS= read -r -p 'Run this now? [y/N] ' answer; then
            answer=""
        fi
    else
        echo "Non-interactive mode. Run the command above manually." >&2
        return 1
    fi

    case "$answer" in
        [Yy]|[Yy][Ee][Ss])
            echo "Installing Go..."
            case "$package_manager" in
                brew)
                    if ! brew install go; then
                        echo "ERROR: Failed to run: brew install go" >&2
                        return 1
                    fi
                    ;;
                apt-get)
                    if ! "${sudo_prefix[@]}" apt-get update; then
                        echo "ERROR: Failed to run: ${sudo_display}apt-get update" >&2
                        return 1
                    fi
                    if ! "${sudo_prefix[@]}" apt-get install -y golang-go; then
                        echo "ERROR: Failed to run: ${sudo_display}apt-get install -y golang-go" >&2
                        return 1
                    fi
                    ;;
                dnf)
                    if ! "${sudo_prefix[@]}" dnf install -y golang; then
                        echo "ERROR: Failed to run: ${sudo_display}dnf install -y golang" >&2
                        return 1
                    fi
                    ;;
                pacman)
                    if ! "${sudo_prefix[@]}" pacman -S --noconfirm go; then
                        echo "ERROR: Failed to run: ${sudo_display}pacman -S --noconfirm go" >&2
                        return 1
                    fi
                    ;;
                apk)
                    if ! "${sudo_prefix[@]}" apk add go; then
                        echo "ERROR: Failed to run: ${sudo_display}apk add go" >&2
                        return 1
                    fi
                    ;;
                *)
                    echo "ERROR: Internal error: unsupported package manager ${package_manager}." >&2
                    return 1
                    ;;
            esac
            ;;
        *)
            echo "Run the command above and re-execute this script." >&2
            exit 130
            ;;
    esac
}

if ! command -v go >/dev/null 2>&1; then
    install_go_prerequisite
fi
check_go_version

DATE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

if (( LOCAL_MODE )); then
    if [[ ! -f go.mod ]] || ! grep -q 'github.com/Dauno/slack-local-agent' go.mod 2>/dev/null; then
        echo "ERROR: --local must be run from the local-agent checkout containing go.mod." >&2
        exit 1
    fi
    proj_dir="$(pwd)"
    if [[ -z "$VERSION" ]]; then
        VERSION="$(git -C "$proj_dir" describe --tags --exact-match HEAD 2>/dev/null || printf 'dev\n')"
    fi
else
    temp_root="$(mktemp -d)"
    release_json="${temp_root}/release.json"
    if [[ -n "$VERSION" ]]; then
        if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][[:alnum:]]+)*$ ]]; then
            printf 'ERROR: Invalid release VERSION: %s\n' "$VERSION" >&2
            exit 1
        fi
        release_api_url="https://api.github.com/repos/${REPO}/releases/tags/${VERSION}"
    else
        release_api_url="https://api.github.com/repos/${REPO}/releases/latest"
    fi

    download_file() {
        local url="$1" output="$2" description="$3" http_status

        if ! http_status="$(curl -fsSL --retry 3 --connect-timeout 10 --max-time 120 \
            -o "$output" -w '%{http_code}' "$url")"; then
            printf 'ERROR: Failed to download %s.\n' "$description" >&2
            return 1
        fi
        if [[ ! "$http_status" =~ ^2[0-9]{2}$ ]]; then
            printf 'ERROR: %s returned HTTP status %s.\n' "$description" "$http_status" >&2
            return 1
        fi
    }

    download_file "$release_api_url" "$release_json" "GitHub release metadata"
    if [[ -z "$VERSION" ]]; then
        if ! VERSION="$(jq -er '.tag_name | strings' "$release_json")"; then
            echo "ERROR: GitHub release metadata did not contain a valid tag_name." >&2
            exit 1
        fi
    fi
    if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][[:alnum:]]+)*$ ]]; then
        printf 'ERROR: GitHub returned an invalid release VERSION: %s\n' "$VERSION" >&2
        exit 1
    fi
    if ! release_tag="$(jq -er '.tag_name | strings' "$release_json")" || [[ "$release_tag" != "$VERSION" ]]; then
        echo "ERROR: GitHub release metadata tag did not match the requested release." >&2
        exit 1
    fi

    archive_name="${BIN}-${VERSION}.tar.gz"
    archive_file="${temp_root}/${archive_name}"
    checksum_name="${archive_name}.sha256"
    checksum_url=""
    archive_url=""
    expected_prefix="https://github.com/${REPO}/releases/download/${VERSION}/"

    if ! archive_url="$(jq -er --arg name "$archive_name" \
        '[.assets[]? | select(.name == $name and (.browser_download_url | type == "string"))] |
         if length == 1 then .[0].browser_download_url else empty end' "$release_json")"; then
        printf 'ERROR: Release %s is missing asset %s.\n' "$VERSION" "$archive_name" >&2
        exit 1
    fi
    if [[ "$archive_url" != "${expected_prefix}${archive_name}" ]]; then
        echo "ERROR: Release archive URL was not the expected versioned GitHub asset." >&2
        exit 1
    fi

    if ! checksum_url="$(jq -er --arg name "$checksum_name" \
        '[.assets[]? | select(.name == $name and (.browser_download_url | type == "string"))] |
         if length == 1 then .[0].browser_download_url else empty end' "$release_json")"; then
        checksum_name=checksums.txt
        if ! checksum_url="$(jq -er --arg name "$checksum_name" \
            '[.assets[]? | select(.name == $name and (.browser_download_url | type == "string"))] |
             if length == 1 then .[0].browser_download_url else empty end' "$release_json")"; then
            printf 'ERROR: Release %s is missing a SHA-256 checksum asset.\n' "$VERSION" >&2
            exit 1
        fi
    fi
    if [[ "$checksum_url" != "${expected_prefix}${checksum_name}" ]]; then
        echo "ERROR: Release checksum URL was not the expected versioned GitHub asset." >&2
        exit 1
    fi

    checksum_file="${temp_root}/${checksum_name}"
    download_file "$archive_url" "$archive_file" "release archive"
    download_file "$checksum_url" "$checksum_file" "release SHA-256 checksum"

    sha256_file() {
        local file="$1"
        if [[ "$checksum_tool" == sha256sum ]]; then
            sha256sum "$file" | sed -n 's/[[:space:]].*$//p'
        else
            shasum -a 256 "$file" | sed -n 's/[[:space:]].*$//p'
        fi
    }

    verify_sha256() {
        local checksum_contents expected actual checksum_line
        checksum_contents="$(<"$checksum_file")"
        expected=""
        if [[ "$checksum_contents" =~ ^[[:space:]]*([[:xdigit:]]{64})([[:space:]]+[*]?([^[:space:]]+))?[[:space:]]*$ ]]; then
            expected="${BASH_REMATCH[1]}"
            if [[ -n "${BASH_REMATCH[3]:-}" && "${BASH_REMATCH[3]}" != "$archive_name" ]]; then
                echo "ERROR: SHA-256 checksum names a different archive." >&2
                return 1
            fi
        else
            if ! checksum_line="$(grep -F -- "$archive_name" "$checksum_file" | \
                grep -E '^[[:space:]]*[[:xdigit:]]{64}[[:space:]]+[*]?[[:space:]]*[^[:space:]]+[[:space:]]*$' || true)"; then
                echo "ERROR: SHA-256 checksum file has an invalid format." >&2
                return 1
            fi
            expected="$(printf '%s\n' "$checksum_line" | sed -n 's/^[[:space:]]*\([[:xdigit:]]\{64\}\).*/\1/p')"
            if [[ ! "$expected" =~ ^[[:xdigit:]]{64}$ ]]; then
                echo "ERROR: SHA-256 checksum file did not contain exactly one matching digest." >&2
                return 1
            fi
        fi

        actual="$(sha256_file "$archive_file")"
        if [[ ! "$actual" =~ ^[[:xdigit:]]{64}$ ]] || \
            ! printf '%s\n' "$actual" | grep -Fqi -- "$expected"; then
            echo "ERROR: Release archive SHA-256 checksum verification failed." >&2
            return 1
        fi
    }

    verify_sha256
    archive_listing="${temp_root}/archive.list"
    if ! tar -tzf "$archive_file" > "$archive_listing"; then
        echo "ERROR: Release archive is not a valid gzip-compressed tar archive." >&2
        exit 1
    fi
    if grep -Eq '(^/|(^|/)\.\.($|/))' "$archive_listing"; then
        echo "ERROR: Release archive contains an unsafe path." >&2
        exit 1
    fi

    source_dir="${temp_root}/source"
    mkdir -p "$source_dir"
    if ! tar -xzf "$archive_file" -C "$source_dir"; then
        echo "ERROR: Unable to extract the verified release archive." >&2
        exit 1
    fi
    source_entries=("$source_dir"/*)
    if [[ ${#source_entries[@]} -ne 1 ]] || [[ ! -d "${source_entries[0]}" ]] || [[ -L "${source_entries[0]}" ]]; then
        echo "ERROR: Release archive has an unexpected layout." >&2
        exit 1
    fi
    proj_dir="${source_entries[0]}"
    if [[ ! -f "${proj_dir}/go.mod" ]]; then
        echo "ERROR: Release archive does not contain a Go module at its root." >&2
        exit 1
    fi
    archive_commit="${proj_dir##*-}"
    if [[ ! "$archive_commit" =~ ^[0-9a-fA-F]{7,40}$ ]]; then
        archive_commit=unknown
    fi
fi

if [[ -z "$COMMIT" ]]; then
    COMMIT="${archive_commit:-$(git -C "$proj_dir" rev-parse --short HEAD 2>/dev/null || printf 'unknown\n')}"
fi
if [[ ! "$VERSION" =~ ^(dev|v[0-9]+\.[0-9]+\.[0-9]+([.-][[:alnum:]]+)*)$ ]]; then
    printf 'ERROR: VERSION has an unsafe format: %s\n' "$VERSION" >&2
    exit 1
fi
if [[ ! "$COMMIT" =~ ^(unknown|[0-9a-fA-F]{7,40})$ ]]; then
    printf 'ERROR: COMMIT has an unsafe format: %s\n' "$COMMIT" >&2
    exit 1
fi
if [[ ! "$DATE" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
    printf 'ERROR: DATE must use UTC format YYYY-MM-DDTHH:MM:SSZ: %s\n' "$DATE" >&2
    exit 1
fi

LDFLAGS="-s -w -X github.com/Dauno/slack-local-agent/internal/buildinfo.Version=${VERSION} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Commit=${COMMIT} -X github.com/Dauno/slack-local-agent/internal/buildinfo.Date=${DATE}"

echo "Building ${BIN}..."
if [[ -z "$temp_root" ]]; then
    temp_root="$(mktemp -d)"
fi
build_dir="${temp_root}/build"
mkdir -p "$build_dir"
go build -C "$proj_dir" -trimpath -ldflags "$LDFLAGS" -o "${build_dir}/${BIN}" ./cmd/local-agent

mkdir -p "$DEST_DIR"
tmp_bin="$(mktemp "${DEST_DIR}/.${BIN}.tmp.XXXXXX")"
install -m 0755 "${build_dir}/${BIN}" "$tmp_bin"
mv -f "$tmp_bin" "${DEST_DIR}/${BIN}"
tmp_bin=""

echo "Installed ${DEST_DIR}/${BIN}"

if [[ ":$PATH:" != *":${DEST_DIR}:"* ]]; then
    echo
    echo "WARNING: ${DEST_DIR} is not in your PATH."
    echo "Add it with:  export PATH=\"\${PATH}:${DEST_DIR}\""
fi
