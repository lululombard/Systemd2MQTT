#!/usr/bin/env bash
# Install or upgrade Systemd2MQTT for the current user from a GitHub release.
#
#   deploy/install.sh                 # latest release
#   deploy/install.sh latest
#   deploy/install.sh v1.2.3          # a specific tag
#   deploy/install.sh --local <dir>   # from an already extracted release tarball
#
# No token is needed for a public repo. For a private repo set GITHUB_TOKEN in the
# environment or put the token in ~/.config/systemd2mqtt/github_token (mode 0600).
#
# What it does:
#   - downloads the tarball for this machine's architecture plus checksums.txt
#   - verifies the sha256 (sha256sum -c --ignore-missing)
#   - installs the binary atomically to ~/.local/bin/systemd2mqtt (.new then mv)
#   - copies config.example.yaml to ~/.config/systemd2mqtt/config.yaml (0600) if missing
#   - installs systemd2mqtt.service and vlc@.service into ~/.config/systemd/user/
#   - systemctl --user daemon-reload, and restarts systemd2mqtt if it is active
#
# Needs: curl, jq, tar, sha256sum. No sudo.
set -euo pipefail

REPO="lululombard/Systemd2MQTT"
BIN_DIR="${HOME}/.local/bin"
CONF_DIR="${HOME}/.config/systemd2mqtt"
UNIT_DIR="${HOME}/.config/systemd/user"
TOKEN_FILE="${CONF_DIR}/github_token"

log() { printf '%s\n' "install.sh: $*" >&2; }
die() { log "error: $*"; exit 1; }

usage() {
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
    exit 0
}

need() {
    for c in "$@"; do
        command -v "$c" >/dev/null 2>&1 || die "missing command: $c (sudo apt install -y curl jq)"
    done
}

# Map uname -m to the goarch used in the archive name.
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo amd64 ;;
        aarch64|arm64) echo arm64 ;;
        *) die "unsupported architecture: $(uname -m)" ;;
    esac
}

# Token is optional; the repo is public. Env wins over the file.
github_token() {
    if [ -n "${GITHUB_TOKEN:-}" ]; then
        printf '%s' "$GITHUB_TOKEN"
    elif [ -r "$TOKEN_FILE" ]; then
        tr -d '[:space:]' < "$TOKEN_FILE"
    fi
}

# curl with the auth header only when we have a token.
gh_curl() {
    local token
    token="$(github_token)"
    if [ -n "$token" ]; then
        curl -fsSL --retry 3 -H "Authorization: Bearer ${token}" "$@"
    else
        curl -fsSL --retry 3 "$@"
    fi
}

# Resolve "latest" or a tag to the release JSON.
release_json() {
    local ref="$1"
    if [ "$ref" = latest ]; then
        gh_curl -H "Accept: application/vnd.github+json" "https://api.github.com/repos/${REPO}/releases/latest"
    else
        gh_curl -H "Accept: application/vnd.github+json" "https://api.github.com/repos/${REPO}/releases/tags/${ref}"
    fi
}

# Download one release asset by name into the target path. Uses the API asset URL so
# it also works on private repos (the browser_download_url needs a session there).
download_asset() {
    local json="$1" name="$2" out="$3" url
    url="$(jq -r --arg n "$name" '.assets[] | select(.name == $n) | .url' <<< "$json")"
    [ -n "$url" ] && [ "$url" != null ] || die "release has no asset named $name"
    gh_curl -H "Accept: application/octet-stream" -o "$out" "$url"
}

# Download and verify a release, echo the extracted directory.
fetch_release() {
    local ref="$1" arch json tag version tarball tmp
    arch="$(detect_arch)"
    log "resolving release ${ref} for linux/${arch}"
    json="$(release_json "$ref")" || die "could not resolve release ${ref} (is the repo public, or is a token set?)"
    tag="$(jq -r .tag_name <<< "$json")"
    [ -n "$tag" ] && [ "$tag" != null ] || die "release JSON has no tag_name"
    version="${tag#v}"
    tarball="systemd2mqtt_${version}_linux_${arch}.tar.gz"

    tmp="$(mktemp -d)"
    log "downloading ${tarball}"
    download_asset "$json" "$tarball" "${tmp}/${tarball}"
    download_asset "$json" checksums.txt "${tmp}/checksums.txt"
    (cd "$tmp" && sha256sum -c --ignore-missing --quiet checksums.txt) || die "checksum mismatch for ${tarball}"
    log "checksum ok"
    mkdir -p "${tmp}/extracted"
    tar -xzf "${tmp}/${tarball}" -C "${tmp}/extracted"
    echo "${tmp}/extracted"
}

# Install from an extracted release directory.
install_from_dir() {
    local src="$1"
    [ -x "${src}/systemd2mqtt" ] || die "no systemd2mqtt binary in ${src}"
    [ -f "${src}/deploy/systemd2mqtt.service" ] || die "no deploy/systemd2mqtt.service in ${src}"

    mkdir -p "$BIN_DIR" "$UNIT_DIR"
    mkdir -p -m 0700 "$CONF_DIR"

    # Atomic replace: the running daemon keeps its old inode until restart.
    install -m 0755 "${src}/systemd2mqtt" "${BIN_DIR}/systemd2mqtt.new"
    mv -f "${BIN_DIR}/systemd2mqtt.new" "${BIN_DIR}/systemd2mqtt"
    log "installed ${BIN_DIR}/systemd2mqtt ($("${BIN_DIR}/systemd2mqtt" --version 2>/dev/null || echo unknown))"

    if [ ! -e "${CONF_DIR}/config.yaml" ]; then
        if [ -f "${src}/config.example.yaml" ]; then
            install -m 0600 "${src}/config.example.yaml" "${CONF_DIR}/config.yaml"
            log "created ${CONF_DIR}/config.yaml from the example, edit the broker and credentials"
        else
            log "no config.example.yaml in ${src}, skipping config creation"
        fi
    else
        log "keeping existing ${CONF_DIR}/config.yaml"
    fi

    install -m 0644 "${src}/deploy/systemd2mqtt.service" "${UNIT_DIR}/systemd2mqtt.service"
    if [ -f "${src}/deploy/vlc@.service" ]; then
        install -m 0644 "${src}/deploy/vlc@.service" "${UNIT_DIR}/vlc@.service"
    fi
    log "installed user units into ${UNIT_DIR}"

    if command -v systemctl >/dev/null 2>&1 && systemctl --user show-environment >/dev/null 2>&1; then
        systemctl --user daemon-reload
        if systemctl --user is-active --quiet systemd2mqtt.service; then
            systemctl --user restart systemd2mqtt.service
            log "restarted systemd2mqtt.service"
        else
            log "systemd2mqtt.service is not running; enable it with: systemctl --user enable --now systemd2mqtt.service"
        fi
    else
        log "no user systemd manager reachable, skipping daemon-reload"
    fi

    case ":${PATH}:" in
        *":${BIN_DIR}:"*) ;;
        *) log "note: ${BIN_DIR} is not in PATH for this shell (the unit uses the absolute path)" ;;
    esac
}

main() {
    local ref=latest local_dir="" tmp=""
    while [ $# -gt 0 ]; do
        case "$1" in
            -h|--help) usage ;;
            --local)
                [ $# -ge 2 ] || die "--local needs a directory"
                local_dir="$2"; shift 2 ;;
            --local=*) local_dir="${1#--local=}"; shift ;;
            -*) die "unknown option $1" ;;
            *) ref="$1"; shift ;;
        esac
    done

    need tar sha256sum install
    if [ -n "$local_dir" ]; then
        [ -d "$local_dir" ] || die "not a directory: $local_dir"
        install_from_dir "$local_dir"
        return
    fi

    need curl jq
    tmp="$(fetch_release "$ref")"
    trap 'rm -rf "$(dirname "$tmp")"' EXIT
    install_from_dir "$tmp"
}

main "$@"
