#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"
# Written by every run: each source resolved to an exact release tag or commit,
# with the SHA-256 of what was downloaded. Published with the aar, it lets a
# release be rebuilt with the same data via `gen_assets.sh pinned <file>`.
LOCKFILE="${DATADIR}/geo-assets.lock"

RULES_REPO="Loyalsoldier/v2ray-rules-dat"
GEOIP_REPO="Loyalsoldier/geoip"


# Check for required dependencies
check_dependencies() {
    command -v jq >/dev/null 2>&1 || { echo >&2 "jq is required but it's not installed. Aborting."; exit 1; }
    command -v go >/dev/null 2>&1 || { echo >&2 "Go is required but it's not installed. Aborting."; exit 1; }
    command -v git >/dev/null 2>&1 || { echo >&2 "git is required but it's not installed. Aborting."; exit 1; }
}


# The tag that github.com/<repo>/releases/latest currently redirects to
latest_release_tag() {
    curl -fsSI "https://github.com/$1/releases/latest" | awk -F/ 'tolower($0) ~ /^location:/ {print $NF}' | tr -d '\r'
}

# The commit a branch currently points at
branch_commit() {
    git ls-remote "https://github.com/$1.git" "refs/heads/$2" | cut -f1
}

sha256() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1
}

# fetch <url> <file> [expected sha256]: download, verify if a checksum is
# given, and print the file's checksum
fetch() {
    curl -fsSL "$1" -o "$2"
    local sum
    sum=$(sha256 "$2")
    if [[ -n "${3:-}" && "$sum" != "$3" ]]; then
        echo >&2 "Checksum mismatch for $2: expected $3, got $sum"
        exit 1
    fi
    echo "$sum"
}

# Download from the sources named by RULES_DAT_TAG and GEOIP_RELEASE_COMMIT,
# verify against any *_SHA256 already set, and record what was fetched
fetch_dat() {
    if [[ ! -d "$DATADIR" ]]; then
        echo "Downloading failed \"$DATADIR\" does not exists"
        exit 1
    fi

    echo "Downloading geoip.dat from $RULES_REPO $RULES_DAT_TAG..."
    GEOIP_DAT_SHA256=$(fetch "https://github.com/$RULES_REPO/releases/download/$RULES_DAT_TAG/geoip.dat" "$DATADIR/geoip.dat" "${GEOIP_DAT_SHA256:-}")

    echo "Downloading geosite.dat from $RULES_REPO $RULES_DAT_TAG..."
    GEOSITE_DAT_SHA256=$(fetch "https://github.com/$RULES_REPO/releases/download/$RULES_DAT_TAG/geosite.dat" "$DATADIR/geosite.dat" "${GEOSITE_DAT_SHA256:-}")

    echo "Downloading geoip-only-cn-private.dat from $GEOIP_REPO $GEOIP_RELEASE_COMMIT..."
    GEOIP_ONLY_CN_PRIVATE_DAT_SHA256=$(fetch "https://raw.githubusercontent.com/$GEOIP_REPO/$GEOIP_RELEASE_COMMIT/geoip-only-cn-private.dat" "$DATADIR/geoip-only-cn-private.dat" "${GEOIP_ONLY_CN_PRIVATE_DAT_SHA256:-}")

    cat > "$LOCKFILE" <<EOF
RULES_DAT_TAG='$RULES_DAT_TAG'
GEOIP_DAT_SHA256='$GEOIP_DAT_SHA256'
GEOSITE_DAT_SHA256='$GEOSITE_DAT_SHA256'
GEOIP_RELEASE_COMMIT='$GEOIP_RELEASE_COMMIT'
GEOIP_ONLY_CN_PRIVATE_DAT_SHA256='$GEOIP_ONLY_CN_PRIVATE_DAT_SHA256'
EOF
    echo "Recorded the exact sources in $LOCKFILE"
}

# Download data function: the newest data, as before, but resolved to an
# exact tag and commit first so that what was downloaded can be recorded
download_dat() {
    RULES_DAT_TAG=$(latest_release_tag "$RULES_REPO")
    GEOIP_RELEASE_COMMIT=$(branch_commit "$GEOIP_REPO" release)
    if [[ -z "$RULES_DAT_TAG" || -z "$GEOIP_RELEASE_COMMIT" ]]; then
        echo >&2 "Could not resolve the latest geo data sources. Aborting."
        exit 1
    fi
    fetch_dat
}

# Exactly the data recorded in a lock file, for rebuilding a release
pinned_dat() {
    local lock="${1:-}"
    if [[ -z "$lock" ]]; then
        echo >&2 "Usage: gen_assets.sh pinned <lock file>"
        exit 1
    fi
    # `.` searches PATH for a bare file name; make sure the given file is read
    [[ "$lock" == */* ]] || lock="./$lock"
    # shellcheck source=/dev/null
    . "$lock"
    fetch_dat
}

# Main execution logic
ACTION="${1:-download}"

check_dependencies

case $ACTION in
    "download") download_dat ;;
    "pinned") pinned_dat "${2:-}" ;;
    *) echo "Invalid action: $ACTION" ; exit 1 ;;
esac
