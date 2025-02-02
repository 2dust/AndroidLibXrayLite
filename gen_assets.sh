#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"

compile_dat () {
    local TMPDIR
    TMPDIR=$(mktemp -d)

    trap 'echo "Aborted, error $? in command: $BASH_COMMAND"; rm -rf $TMPDIR; exit 1' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"
    if [[ -d ${GEOSITE} ]]; then
        git -C "${GEOSITE}" pull
    else
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
    fi
    go run "${GEOSITE}/main.go"

    if [[ -e "${GEOSITE}/dlc.dat" ]]; then
        mv -f "${GEOSITE}/dlc.dat" "${DATADIR}/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    if [[ ! -x "${GOPATH}/bin/geoip" ]]; then
        go install github.com/v2ray/geoip@latest
    fi

    cd "${TMPDIR}"
    curl -sL -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir -p geoip && find . -name '*.csv' -exec mv -t ./geoip {} +

    "${GOPATH}/bin/geoip" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    if [[ -e geoip.dat ]]; then
        mv -f geoip.dat "${DATADIR}/geoip.dat"
        echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi

    rm -rf "${TMPDIR}"
}

download_dat () {
    local geoip_url
    local geosite_url

    geoip_url=$(wget -qO - https://api.github.com/repos/dyhkwong/v2ray-geoip/releases/latest | jq -r '.assets[].browser_download_url' | grep geoip.dat)
    wget -q "${geoip_url}" -O "${DATADIR}/geoip.dat"

    geosite_url=$(wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest | jq -r '.assets[].browser_download_url' | grep geosite.dat)
    wget -q "${geosite_url}" -O "${DATADIR}/geosite.dat"
}

ACTION="${1:-download}"

case "${ACTION}" in
    "download") download_dat ;;
    "compile") compile_dat ;;
    *) echo "Unknown action: ${ACTION}" ;;
esac
