#!/bin/bash

set -euo pipefail

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATADIR="${__dir}/data"

handle_error() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    exit 1
}

trap handle_error ERR

update_file() {
    local src_file="$1"
    local dest_file="$2"
    
    if [[ -e "$src_file" ]]; then
        rm -f "$dest_file"
        mv "$src_file" "$dest_file"
        echo "----------> ${dest_file} updated."
    else
        echo "----------> ${dest_file} failed to update."
    fi
}

compile_dat() {
    local TMPDIR
    TMPDIR=$(mktemp -d)
    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

    # Clone or update the geosite repository
    if [[ -d "${GEOSITE}" ]]; then
        (cd "${GEOSITE}" && git pull)
    else
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
    fi

    (cd "${GEOSITE}" && go run main.go)
    update_file "dlc.dat" "$DATADIR/geosite.dat"

    # Ensure geoip is installed
    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        go get -v -u github.com/v2ray/geoip
    fi

    # Download and process GeoLite data
    (cd "$TMPDIR" && {
        curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
        unzip -q GeoLite2-Country-CSV.zip
        mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +
        
        "$GOPATH/bin/geoip" \
            --country=./geoip/GeoLite2-Country-Locations-en.csv \
            --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
            --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv
    })

    update_file "geoip.dat" "$DATADIR/geoip.dat"
    
    rm -rf "$TMPDIR"
}

download_dat() {
    local geoip_url=$(wget -qO - https://api.github.com/repos/v2ray/geoip/releases/latest | grep browser_download_url | cut -d '"' -f 4)
    local geosite_url=$(wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest | grep browser_download_url | cut -d '"' -f 4)

    wget -qO "$DATADIR/geoip.dat" "$geoip_url"
    wget -qO "$DATADIR/geosite.dat" "$geosite_url"
}

ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
esac
