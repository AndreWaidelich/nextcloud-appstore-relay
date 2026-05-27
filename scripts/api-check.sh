#!/usr/bin/env bash
# Sanity-check that the upstream Nextcloud App Store API still has the shape
# this relay assumes. Run BEFORE deploying the relay (and as a periodic canary).
#
# Exit codes:
#   0 -- upstream looks good, relay should work
#   1 -- usage / missing dependency
#   2 -- upstream unreachable or non-200
#   3 -- JSON malformed or schema unexpected
#
# Usage:
#   scripts/api-check.sh                   # uses default upstream
#   scripts/api-check.sh https://mirror... # checks a custom upstream
#   RELAY_UPSTREAM=https://... scripts/api-check.sh

set -euo pipefail

UPSTREAM="${1:-${RELAY_UPSTREAM:-https://apps.nextcloud.com/api/v1}}"
UPSTREAM="${UPSTREAM%/}"

need() {
    command -v "$1" >/dev/null 2>&1 || {
        echo "missing dependency: $1 (apt install $1)" >&2
        exit 1
    }
}
need curl
need jq

say()  { printf '\033[1m== %s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m  %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m  %s\n' "$*" >&2; exit 3; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*" >&2; }

say "Upstream: $UPSTREAM"

# --- 1. reachability ----------------------------------------------------------
say "Reachability (apps.json, categories.json)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

http_apps=$(curl -sS -L -o "$tmp/apps.json" -w '%{http_code}' \
    --max-time 60 "$UPSTREAM/apps.json" || true)
[[ "$http_apps" == "200" ]] || {
    echo "  HTTP $http_apps from $UPSTREAM/apps.json" >&2
    exit 2
}
ok "apps.json HTTP 200 ($(wc -c < "$tmp/apps.json" | tr -d ' ') bytes)"

http_cat=$(curl -sS -L -o "$tmp/categories.json" -w '%{http_code}' \
    --max-time 30 "$UPSTREAM/categories.json" || true)
[[ "$http_cat" == "200" ]] || {
    echo "  HTTP $http_cat from $UPSTREAM/categories.json" >&2
    exit 2
}
ok "categories.json HTTP 200"

# --- 2. JSON validity ---------------------------------------------------------
say "JSON shape"
jq -e '.' "$tmp/apps.json"        >/dev/null 2>&1 || fail "apps.json is not valid JSON"
jq -e '.' "$tmp/categories.json"  >/dev/null 2>&1 || fail "categories.json is not valid JSON"
ok "both files parse as JSON"

jq -e 'type == "array"' "$tmp/apps.json"       >/dev/null 2>&1 || fail "apps.json top-level is not an array"
jq -e 'type == "array"' "$tmp/categories.json" >/dev/null 2>&1 || fail "categories.json top-level is not an array"
ok "both top-level shapes are arrays (apps=$(jq 'length' "$tmp/apps.json"), categories=$(jq 'length' "$tmp/categories.json"))"

# --- 3. schema fields the relay actually needs --------------------------------
say "Required fields on apps[].releases[]"
missing=$(jq -r '
    [ .[]
      | .id as $id
      | .certificate as $cert
      | .releases[]?
      | select((.download | type) != "string"
            or (.signature | type) != "string"
            or (.signatureDigest | type) != "string"
            or ($cert | type) != "string")
      | $id
    ] | unique | join(",")
' "$tmp/apps.json")

if [[ -n "$missing" ]]; then
    warn "some apps are missing required fields (download/signature/signatureDigest/certificate):"
    echo "$missing" | tr ',' '\n' | sed 's/^/    - /' >&2
    # not fatal: the relay skips these per-release; warn but pass
else
    ok "every release has download, signature, signatureDigest; every app has certificate"
fi

# --- 4. signature digest is what we expect (sha512) ---------------------------
digests=$(jq -r '
    [ .[].releases[].signatureDigest | select(. != null and . != "") ]
    | unique | join(",")
' "$tmp/apps.json")
if [[ "$digests" == "sha512" ]]; then
    ok "every release uses sha512 digest"
else
    warn "unexpected signatureDigest values: $digests (the relay still passes them through verbatim)"
fi

# --- 5. download hosts look like external tarballs ----------------------------
read -r total_releases gh_releases distinct_hosts < <(jq -r '
    [ .[].releases[].download | capture("https?://(?<h>[^/]+)").h ] as $hs
    | [($hs | length), ($hs | map(select(. == "github.com")) | length), ($hs | unique | length)]
    | @tsv
' "$tmp/apps.json")
ok "found $total_releases releases across $distinct_hosts distinct hosts ($gh_releases on github.com)"

say "Result: upstream API matches what the relay expects."
