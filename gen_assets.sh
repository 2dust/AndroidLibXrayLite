#!/bin/bash

# Exit immediately if a command exits with a non-zero status
set -o errexit
# Fail the entire pipeline if any command in the pipeline fails
set -o pipefail
# Treat unset variables as an error when substituting
set -o nounset

# Set magic variables for current file & directory
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DATADIR="${__dir}/data"
GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

# Function to handle errors and cleanup
error_handler() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -n "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
    exit 1
}

trap error_handler ERR

# Function to compile data
compile_dat() {
    TMPDIR=$(mktemp -d)
    cd "$TMPDIR"

    # Clone or update the domain-list-community repository
    if [[ -d $GEOSITE ]]; then
        cd "$GEOSITE" && git pull
    else
        git clone https://github.com/v2ray/domain-list-community.git "$GEOSITE"
        cd "$GEOSITE"
    fi

    # Run the main Go program to generate geosite.dat
    go run main.go

    # Update geosite.dat if it exists
    update_file "dlc.dat" "geosite.dat" "$DATADIR"

    # Install geoip if not already installed
    [[ ! -x "$GOPATH/bin/geoip" ]] && go get -v -u github.com/v2ray/geoip

    # Download and process GeoLite2 country database
    download_geoip_data

    # Update geoip.dat if it exists
    update_file "geoip.dat" "geoip.dat" "$DATADIR"

    rm -rf "$TMPDIR"
}

# Function to update files and notify user
update_file() {
    local src_file="$1"
    local dest_file="$2"
    local dest_dir="$3"

    if [[ -e $src_file ]]; then
        mv -f "$src_file" "$dest_dir/$dest_file"
        echo "----------> $dest_file updated."
    else
        echo "----------> $dest_file failed to update."
    fi
}

# Function to download and process GeoLite2 data
download_geoip_data() {
    curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip

    mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +

    $GOPATH/bin/geoip \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv
}

# Function to download data directly from GitHub releases
download_dat() {
    for repo in "geoip" "domain-list-community"; do
        wget -qO - "https://api.github.com/repos/v2ray/${repo}/releases/latest" \
            | grep browser_download_url | cut -d '"' -f 4 \
            | wget -i - -O "$DATADIR/${repo}.dat"
    done
}

# Determine action based on input argument or default to download
ACTION="${1:-download}"

case $ACTION in 
    "download") download_dat ;; 
    "compile") compile_dat ;; 
esac
