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
error_handler() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    rm -rf "$TMPDIR"
    exit 1
}

# Compile data from the domain list community and GeoIP
compile_dat() {
    TMPDIR=$(mktemp -d)
    trap error_handler ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"
    
    # Clone or update the geosite repository
    if [[ -d ${GEOSITE} ]]; then
        cd "${GEOSITE}" && git pull
    else
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
        cd "${GEOSITE}"
    fi
    
    go run main.go

    if [[ -e dlc.dat ]]; then
        mv -f dlc.dat "$DATADIR/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    # Ensure geoip binary is available
    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        go get -v -u github.com/v2ray/geoip
    fi

    # Download and process GeoIP data
    cd "$TMPDIR"
    curl -LO http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir geoip && mv ./*.csv geoip/

    "$GOPATH/bin/geoip" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    if [[ -e geoip.dat ]]; then
        mv -f geoip.dat "$DATADIR/geoip.dat"
        echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi

    trap - ERR  # Reset the trap on success
}

# Download data from GitHub releases if compile is not requested
download_dat() {
    for repo in "geoip" "domain-list-community"; do
        wget -qO - "https://api.github.com/repos/v2ray/${repo}/releases/latest" \
            | grep browser_download_url | cut -d '"' -f 4 \
            | wget -i - -O "$DATADIR/${repo}.dat"
    done
}

# Main execution logic
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
esac
