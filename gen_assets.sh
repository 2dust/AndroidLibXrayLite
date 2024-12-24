#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATADIR="${__dir}/data"

# Function to handle errors
error_exit() {
    echo "Aborted, error $? in command: $BASH_COMMAND"
    exit 1
}

trap error_exit ERR

# Function to update a repository
update_repo() {
    local repo_dir="$1"
    local repo_url="$2"

    if [[ -d "${repo_dir}" ]]; then
        (cd "${repo_dir}" && git pull)
    else
        git clone "${repo_url}" "${repo_dir}"
    fi
}

# Function to compile geosite and geoip data
compile_dat() {
    local TMPDIR
    TMPDIR=$(mktemp -d)

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"
    update_repo "${GEOSITE}" "https://github.com/v2ray/domain-list-community.git"

    go run main.go

    # Update geosite.dat if dlc.dat exists
    [[ -e dlc.dat ]] && mv -f dlc.dat "${DATADIR}/geosite.dat" && echo "----------> geosite.dat updated." || echo "----------> geosite.dat failed to update."

    # Ensure geoip is installed
    command -v geoip >/dev/null || go get -v -u github.com/v2ray/geoip

    cd "$TMPDIR"
    curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip

    mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +

    # Generate geoip.dat from the CSV files
    "${GOPATH}/bin/geoip" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    # Update geoip.dat if it exists
    [[ -e geoip.dat ]] && mv -f geoip.dat "${DATADIR}/geoip.dat" && echo "----------> geoip.dat updated." || echo "----------> geoip.dat failed to update."

    rm -rf "$TMPDIR"  # Clean up temporary directory after use
}

# Function to download data from GitHub releases using jq for better parsing
download_dat() {
    local urls=(
        "https://api.github.com/repos/v2ray/geoip/releases/latest"
        "https://api.github.com/repos/v2ray/domain-list-community/releases/latest"
    )

    for url in "${urls[@]}"; do
        local download_url=$(curl -s "$url" | jq -r '.assets[].browser_download_url' | head -n 1)
        wget -P "${DATADIR}" "$download_url"
    done
}

# Main script execution
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;
esac
