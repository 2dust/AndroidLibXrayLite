#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Define magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"

compile_dat() {
    local TMPDIR
    TMPDIR=$(mktemp -d)

    trap 'echo -e "Aborted, error $? in command: $BASH_COMMAND"; rm -rf "$TMPDIR"; exit 1' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"
    
    # Update or clone the geosite repository
    if [[ -d "${GEOSITE}" ]]; then
        (cd "${GEOSITE}" && git pull)
    else
        mkdir -p "${GEOSITE}"
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
    fi
    
    (cd "${GEOSITE}" && go run main.go)

    # Update geosite.dat
    if [[ -e dlc.dat ]]; then
        rm -f "${DATADIR}/geosite.dat"
        mv dlc.dat "${DATADIR}/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    # Ensure geoip is installed
    if [[ ! -x "${GOPATH}/bin/geoip" ]]; then
        go get -v -u github.com/v2ray/geoip
    fi

    # Download and process GeoLite2 data
    (cd "$TMPDIR" && {
        curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
        unzip -q GeoLite2-Country-CSV.zip
        mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +
        
        "${GOPATH}/bin/geoip" \
            --country=./geoip/GeoLite2-Country-Locations-en.csv \
            --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
            --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv
    })

    # Update geoip.dat
    if [[ -e geoip.dat ]]; then
        rm -f "${DATADIR}/geoip.dat"
        mv geoip.dat "${DATADIR}/geoip.dat"
        echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi

    # Clean up temporary directory
    rm -rf "$TMPDIR"
}

download_dat() {
    wget -qO - https://api.github.com/repos/v2ray/geoip/releases/latest \
        | grep browser_download_url | cut -d '"' -f 4 \
        | wget -i - -O "${DATADIR}/geoip.dat"

    wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest \
        | grep browser_download_url | cut -d '"' -f 4 \
        | wget -i - -O "${DATADIR}/geosite.dat"
}

# Determine action based on input argument or default to download
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
esac
