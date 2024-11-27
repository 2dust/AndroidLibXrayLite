#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATADIR="${__dir}/data"

# Function to handle errors and cleanup
error_handler() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -d "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
    exit 1
}

trap error_handler ERR

# Function to update data files from a Git repository
update_git_repo() {
    local dir="$1"
    local repo_url="$2"
    
    if [[ -d "${dir}" ]]; then
        cd "${dir}" && git pull
    else
        mkdir -p "${dir}"
        git clone "${repo_url}" "${dir}"
    fi
}

# Function to compile geosite and geoip data
compile_data() {
    TMPDIR=$(mktemp -d)
    
    local geosite_dir="${GOPATH}/src/github.com/v2ray/domain-list-community"
    local geoip_bin="$GOPATH/bin/geoip"

    update_git_repo "${geosite_dir}" "https://github.com/v2ray/domain-list-community.git"
    
    cd "${geosite_dir}"
    go run main.go

    if [[ -e dlc.dat ]]; then
        mv dlc.dat "$DATADIR/geosite.dat" && echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    if [[ ! -x "${geoip_bin}" ]]; then
        go get -v -u github.com/v2ray/geoip
    fi

    # Download and process GeoIP data
    cd "$TMPDIR"
    curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +

    "${geoip_bin}" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    if [[ -e geoip.dat ]]; then
        mv geoip.dat "$DATADIR/geoip.dat" && echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi

    rm -rf "$TMPDIR"
}

# Function to download data files from GitHub releases
download_data() {
    local geoip_url="https://api.github.com/repos/v2ray/geoip/releases/latest"
    local geosite_url="https://api.github.com/repos/v2ray/domain-list-community/releases/latest"

    wget -qO - "$geoip_url" | grep browser_download_url | cut -d '"' -f 4 | wget -i - -O "$DATADIR/geoip.dat"
    
    wget -qO - "$geosite_url" | grep browser_download_url | cut -d '"' -f 4 | wget -i - -O "$DATADIR/geosite.dat"
}

# Main execution flow
ACTION="${1:-download}"

case $ACTION in
    "download") download_data ;;
    "compile") compile_data ;;
esac
