#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"
ASSETSDIR="${__dir}/assets"

# Function to handle errors
error_exit() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -d "$TMPDIR" ]] && rm -rf "$TMPDIR"
    exit 1
}

# Function to ensure directory exists
ensure_dir() {
    local dir=$1
    [[ ! -d "$dir" ]] && mkdir -p "$dir"
}

# Function to update or clone a repository
update_or_clone_repo() {
    local repo_dir=$1
    local repo_url=$2
    if [[ -d "$repo_dir" ]]; then
        (cd "$repo_dir" && git pull)
    else
        git clone "$repo_url" "$repo_dir"
    fi
}

# Compile data function
compile_dat() {
    TMPDIR=$(mktemp -d)
    trap 'error_exit' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

    # Clone or update the geosite repository
    update_or_clone_repo "$GEOSITE" "https://github.com/v2ray/domain-list-community.git"
    
    (cd "$GEOSITE" && go run main.go)

    # Update geosite.dat if dlc.dat exists
    if [[ -e "${GEOSITE}/dlc.dat" ]]; then
        mv -f "${GEOSITE}/dlc.dat" "$DATADIR/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    # Install geoip if not already installed
    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        go get -v -u github.com/v2ray/geoip
    fi

    cd "$TMPDIR"
    
    # Download and process GeoLite2 data
    curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir geoip && mv *.csv geoip/

    "$GOPATH/bin/geoip" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    # Update geoip.dat if it exists
    if [[ -e geoip.dat ]]; then
        mv -f geoip.dat "$DATADIR/geoip.dat"
        echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi
    
    rm -rf "$TMPDIR"
    trap - ERR  # Disable error trap
}

# Download data function
download_dat() {
    ensure_dir "$DATADIR"
    
    wget -qO - https://api.github.com/repos/dyhkwong/v2ray-geoip/releases/latest \
        | jq -r .assets[].browser_download_url | grep geoip.dat \
        | xargs wget -O "$DATADIR/geoip.dat"

    wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest \
        | grep browser_download_url | cut -d '"' -f 4 \
        | xargs wget -O "$DATADIR/geosite.dat"
}

# Ensure necessary directories exist
ensure_dir "$ASSETSDIR"
ensure_dir "$DATADIR"

# Main execution logic
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
    *) echo "Invalid action: $ACTION"; exit 1 ;;
esac
