#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"

# Function to handle errors
error_exit() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -d "$TMPDIR" ]] && rm -rf "$TMPDIR"
    exit 1
}

# Check for required dependencies
check_dependencies() {
    command -v jq >/dev/null 2>&1 || { echo >&2 "jq is required but it's not installed. Aborting."; exit 1; }
    command -v go >/dev/null 2>&1 || { echo >&2 "Go is required but it's not installed. Aborting."; exit 1; }
}

# Compile data function
compile_dat() {
    TMPDIR=$(mktemp -d "${TMPDIR:-/tmp}/compile_dat.XXXXXX")
    trap 'error_exit' ERR

    local GEOSITE="${GOPATH}/src/github.com/Loyalsoldier/v2ray-rules-dat"

    # Clone or update the geosite repository
    if [[ -d ${GEOSITE} ]]; then
        echo "Updating geosite repository..."
        (cd "${GEOSITE}" && git pull)
    else
        echo "Cloning geosite repository..."
        git clone https://github.com/Loyalsoldier/v2ray-rules-dat.git "${GEOSITE}"
    fi
    
    echo "Running geosite generation..."
    (cd "${GEOSITE}" && go run main.go)

    # Update geosite.dat if dlc.dat exists
    if [[ -e "${GEOSITE}/dlc.dat" ]]; then
        mv -f "${GEOSITE}/dlc.dat" "$DATADIR/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    # Install geoip if not already installed
    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        echo "Installing geoip..."
        go install github.com/Loyalsoldier/geoip@latest
    fi

    cd "$TMPDIR"

    # Download and process GeoLite2 data
    echo "Downloading GeoLite2 data..."
    curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir geoip && mv *.csv geoip/

    echo "Generating geoip.dat..."
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

    trap - ERR  # Disable error trap
}

# Download data function
download_dat() {
    echo "Downloading geoip.dat..."
    wget -qO - https://api.github.com/repos/Loyalsoldier/v2ray-rules-dat/releases/latest \
        | jq -r .assets[].browser_download_url | grep geoip.dat \
        | xargs wget -O "$DATADIR/geoip.dat"

    echo "Downloading geosite.dat..."
    wget -qO - https://api.github.com/repos/Loyalsoldier/v2ray-rules-dat/releases/latest \
        | grep browser_download_url | cut -d '"' -f 4 \
        | xargs wget -O "$DATADIR/geosite.dat"
}

# Main execution logic
ACTION="${1:-download}"

check_dependencies

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
    *) echo "Invalid action: $ACTION" ; exit 1 ;;
esac
