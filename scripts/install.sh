#!/usr/bin/env bash
# One-shot installer for nextcloud-appstore-relay on Ubuntu / Debian / RHEL.
#
# What it does (each step is idempotent):
#   1. detects OS family + package manager
#   2. installs missing dependencies (make, curl, jq, docker, docker compose)
#   3. asks for the relay's public URL (or takes it from --public-url)
#   4. writes .env from .env.example
#   5. runs scripts/api-check.sh against the upstream
#   6. builds and starts the relay via docker compose
#   7. prints the Nextcloud-side commands you still need to run
#
# Usage:
#   sudo bash scripts/install.sh
#   sudo bash scripts/install.sh --public-url http://10.0.0.5:8080
#   sudo bash scripts/install.sh --public-url http://relay.lan:8080 --yes
#
# Re-runs are safe: the script only changes things that aren't already correct.

set -euo pipefail

REPO_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." &>/dev/null && pwd)"
cd "$REPO_DIR"

PUBLIC_URL=""
ASSUME_YES=0
SKIP_API_CHECK=0

while [[ $# -gt 0 ]]; do
    case "$1" in
        --public-url) PUBLIC_URL="$2"; shift 2 ;;
        --yes|-y)     ASSUME_YES=1; shift ;;
        --skip-api-check) SKIP_API_CHECK=1; shift ;;
        -h|--help)
            sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *) echo "unknown arg: $1" >&2; exit 1 ;;
    esac
done

# ---- helpers -----------------------------------------------------------------
say()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m  %s\n' "$*"; }
info() { printf '  \033[36m..\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*" >&2; }
die()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*" >&2; exit 1; }

need_root() {
    if [[ $EUID -ne 0 ]]; then
        if command -v sudo >/dev/null 2>&1; then
            SUDO="sudo"
        else
            die "this step needs root, and sudo is not installed"
        fi
    else
        SUDO=""
    fi
}

confirm() {
    [[ $ASSUME_YES -eq 1 ]] && return 0
    local prompt="$1"
    read -r -p "  $prompt [y/N] " ans
    [[ "$ans" =~ ^[Yy]$ ]]
}

# ---- 1. OS + package manager detection ---------------------------------------
say "OS check"
if [[ ! -f /etc/os-release ]]; then
    die "/etc/os-release missing -- cannot determine OS"
fi
# shellcheck disable=SC1091
. /etc/os-release

FAMILY=""
case "${ID:-} ${ID_LIKE:-}" in
    *ubuntu*|*debian*)
        FAMILY=debian ;;
    *rhel*|*fedora*|*centos*|*rocky*|*almalinux*)
        FAMILY=rhel ;;
    *)
        warn "this script targets Debian/Ubuntu and RHEL/Fedora/Rocky/Alma; detected '${PRETTY_NAME:-$ID}'"
        confirm "continue anyway?" || die "aborted"
        # Best-effort fallback based on which package manager exists.
        if command -v apt-get >/dev/null 2>&1; then FAMILY=debian
        elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then FAMILY=rhel
        else die "no supported package manager found"
        fi
        ;;
esac

PKG=""
if [[ $FAMILY == debian ]]; then
    PKG=apt
elif command -v dnf >/dev/null 2>&1; then
    PKG=dnf
elif command -v yum >/dev/null 2>&1; then
    PKG=yum
else
    die "no supported package manager (apt, dnf, yum) found"
fi

ok "detected ${PRETTY_NAME:-$ID} (family=$FAMILY, pkg=$PKG)"

# ---- 2. dependencies ---------------------------------------------------------
say "Dependencies"
need_root

pkg_install() {
    case "$PKG" in
        apt)
            info "apt-get install: $*"
            $SUDO env DEBIAN_FRONTEND=noninteractive apt-get update -qq
            $SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
            ;;
        dnf)
            info "dnf install: $*"
            $SUDO dnf install -y -q "$@"
            ;;
        yum)
            info "yum install: $*"
            $SUDO yum install -y -q "$@"
            ;;
    esac
}

pkg_available() {
    case "$PKG" in
        apt)  apt-cache show "$1" >/dev/null 2>&1 ;;
        dnf)  $SUDO dnf list "$1" >/dev/null 2>&1 ;;
        yum)  $SUDO yum list "$1" >/dev/null 2>&1 ;;
    esac
}

# Basic tools: make, curl, jq
missing=()
command -v make >/dev/null 2>&1 || missing+=(make)
command -v curl >/dev/null 2>&1 || missing+=(curl)
command -v jq   >/dev/null 2>&1 || missing+=(jq)
if [[ ${#missing[@]} -gt 0 ]]; then
    pkg_install "${missing[@]}"
fi
ok "make + curl + jq present"

# ---- 3. Docker + compose -----------------------------------------------------
install_docker_debian() {
    info "installing docker.io from the distro repo"
    pkg_install docker.io
}

install_compose_debian() {
    info "installing docker compose plugin"
    if pkg_available docker-compose-v2; then
        pkg_install docker-compose-v2
    elif pkg_available docker-compose-plugin; then
        pkg_install docker-compose-plugin
    else
        die "neither docker-compose-v2 nor docker-compose-plugin is available via apt; install Docker's official package per https://docs.docker.com/engine/install/"
    fi
}

install_docker_rhel() {
    info "adding Docker's official RHEL repo"
    pkg_install dnf-plugins-core || pkg_install yum-utils
    # rhel | fedora | centos -- match the right repo file
    case "${ID:-}" in
        fedora)
            $SUDO dnf config-manager --add-repo https://download.docker.com/linux/fedora/docker-ce.repo
            ;;
        rhel|rocky|almalinux|centos|*)
            $SUDO dnf config-manager --add-repo https://download.docker.com/linux/rhel/docker-ce.repo \
                || $SUDO dnf config-manager --add-repo https://download.docker.com/linux/centos/docker-ce.repo
            ;;
    esac
    info "installing docker-ce + docker compose plugin"
    pkg_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
}

if ! command -v docker >/dev/null 2>&1; then
    if [[ $FAMILY == debian ]]; then
        install_docker_debian
    else
        install_docker_rhel
    fi
fi
ok "docker present ($(docker --version 2>/dev/null || echo unknown))"

if ! docker compose version >/dev/null 2>&1; then
    if [[ $FAMILY == debian ]]; then
        install_compose_debian
    else
        # Docker CE on RHEL ships docker-compose-plugin; if not present, retry.
        pkg_install docker-compose-plugin || die "docker compose plugin missing"
    fi
fi
ok "docker compose plugin present ($(docker compose version --short 2>/dev/null || echo unknown))"

if ! $SUDO systemctl is-active --quiet docker; then
    info "starting docker service"
    $SUDO systemctl enable --now docker
fi
ok "docker service is active"

# Note: we don't add the invoking user to the docker group here. If you want
# rootless docker invocations, run: sudo usermod -aG docker $USER  (then log out/in).

# ---- 4. relay configuration --------------------------------------------------
say "Relay configuration (.env)"
if [[ ! -f .env.example ]]; then
    die ".env.example missing -- are you running this from the repo root?"
fi

if [[ -z "$PUBLIC_URL" && -f .env ]]; then
    existing=$(grep -E '^RELAY_PUBLIC_URL=' .env | head -1 | cut -d= -f2-)
    if [[ -n "$existing" && "$existing" != "https://relay.example.org" ]]; then
        info ".env already configured with RELAY_PUBLIC_URL=$existing"
        PUBLIC_URL="$existing"
    fi
fi

if [[ -z "$PUBLIC_URL" ]]; then
    # Suggest a sensible default: the host's primary IP on port 8080.
    default_ip=$(hostname -I 2>/dev/null | awk '{print $1}')
    default_url="http://${default_ip:-127.0.0.1}:8080"
    if [[ $ASSUME_YES -eq 1 ]]; then
        PUBLIC_URL="$default_url"
        info "using default RELAY_PUBLIC_URL=$PUBLIC_URL (--yes set)"
    else
        echo "  Enter the URL at which your Nextcloud instance will reach this relay."
        echo "  Example: http://10.0.0.5:8080 or https://appstore-relay.example.org"
        read -r -p "  RELAY_PUBLIC_URL [$default_url]: " entered
        PUBLIC_URL="${entered:-$default_url}"
    fi
fi

# Basic shape check
if ! [[ "$PUBLIC_URL" =~ ^https?://[^/]+ ]]; then
    die "RELAY_PUBLIC_URL '$PUBLIC_URL' must start with http:// or https:// and have a host"
fi
PUBLIC_URL="${PUBLIC_URL%/}"
ok "using RELAY_PUBLIC_URL=$PUBLIC_URL"

if [[ ! -f .env ]]; then
    cp .env.example .env
    ok "wrote .env from .env.example"
fi
# Idempotently set RELAY_PUBLIC_URL
if grep -qE '^RELAY_PUBLIC_URL=' .env; then
    # portable sed -i across linux/macOS
    if sed --version >/dev/null 2>&1; then
        sed -i -E "s|^RELAY_PUBLIC_URL=.*|RELAY_PUBLIC_URL=${PUBLIC_URL}|" .env
    else
        sed -i '' -E "s|^RELAY_PUBLIC_URL=.*|RELAY_PUBLIC_URL=${PUBLIC_URL}|" .env
    fi
else
    echo "RELAY_PUBLIC_URL=${PUBLIC_URL}" >> .env
fi
ok ".env updated"

# ---- 5. upstream sanity ------------------------------------------------------
if [[ $SKIP_API_CHECK -eq 0 ]]; then
    say "Upstream API sanity check"
    bash scripts/api-check.sh
else
    warn "skipping upstream API check (--skip-api-check)"
fi

# ---- 6. build + start --------------------------------------------------------
say "Build + start"
$SUDO docker compose up -d --build

# wait briefly for /healthz
listen_port=$(grep -E '^RELAY_LISTEN=' .env | tail -1 | cut -d= -f2- | sed -E 's/^.*://')
listen_port="${listen_port:-8080}"
for i in $(seq 1 15); do
    if curl -sf "http://127.0.0.1:${listen_port}/healthz" >/dev/null; then
        ok "relay healthy on http://127.0.0.1:${listen_port}"
        break
    fi
    sleep 1
    [[ $i -eq 15 ]] && warn "relay did not become healthy in 15s (check: docker compose logs)"
done

# ---- 7. next steps -----------------------------------------------------------
say "Next steps on the Nextcloud host"
cat <<EOF
  On the Nextcloud host (not here), run one of:

      # bare metal / snap
      sudo -u www-data php occ config:system:set appstoreurl --value="${PUBLIC_URL}"

      # nextcloud in docker
      sudo docker exec -u www-data <nextcloud-container> \\
          php occ config:system:set appstoreurl --value="${PUBLIC_URL}"

  Useful commands here on the relay host:

      make logs       # follow container logs
      make status     # container status + cache size
      make api-check  # re-verify upstream API shape
      make restart    # apply .env changes
      make update     # pull repo + rebuild + restart
EOF
