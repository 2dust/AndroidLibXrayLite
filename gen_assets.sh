#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATADIR="${__dir}/data"

# Error handling function
handle_error() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -n "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
    exit 1
}

trap handle_error ERR

# Function to update a file from source to destination
update_file() {
    local src_file="$1"
    local dest_file="$DATADIR/$2"
    
    if [[ -e "$src_file" ]]; then
        rm -f "$dest_file"
        mv "$src_file" "$dest_file"
        echo "----------> $dest_file updated."
    else
        echo "----------> $dest_file failed to update."
    fi
}

# Function to compile data files
compile_data() {
    TMPDIR=$(mktemp -d)
    local geosite_dir="${GOPATH}/src/github.com/v2ray/domain-list-community"

    # Clone or update the geosite repository
    if [[ -d ${geosite_dir} ]]; then
        (cd "${geosite_dir}" && git pull)
    else
        git clone https://github.com/v2ray/domain-list-community.git "${geosite_dir}"
    fi
    
    (cd "${geosite_dir}" && go run main.go)

    update_file "dlc.dat" "geosite.dat"

    # Install geoip if not installed
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

        update_file "geoip.dat" "geoip.dat"
    })

    [[ -n "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
}

# Function to download data files from GitHub releases
download_data() {
    for repo in "geoip" "domain-list-community"; do
        local download_url
        download_url=$(wget -qO - "https://api.github.com/repos/v2ray/${repo}/releases/latest" \
            | grep browser_download_url | cut -d '"' -f 4)
        
        wget -O "$DATADIR/${repo}.dat" "$download_url"
    done
}

# Main script execution logic
ACTION="${1:-download}"

case $ACTION in
    "download") download_data ;;
    "compile") compile_data ;;
esac
