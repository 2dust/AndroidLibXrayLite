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

# Compile data function
compile_dat() {
    TMPDIR=$(mktemp -d)
    trap 'error_exit' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

# Clone or update the geosite repository
    if [[ -d ${GEOSITE} ]]; then
        (cd "${GEOSITE}" && git pull)
    else
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
    fi
    
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

    trap - ERR  # Disable error trap
}

# Download data function
download_dat() {
    wget -qO - https://api.github.com/repos/v2ray/geoip/releases/latest \
        | jq -r .assets[].browser_download_url | grep geoip.dat \
        | xargs wget -O "$DATADIR/geoip.dat"

    wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest \
        | grep browser_download_url | cut -d '"' -f 4 \
        | xargs wget -O "$DATADIR/geosite.dat"
}

# Main execution logic
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
esac
