#!/bin/bash

set -o errexit  # Exit immediately if a command exits with a non-zero status
set -o pipefail # Return the exit status of the last command in the pipeline that failed
set -o nounset  # Treat unset variables as an error and exit immediately

# Set magic variables for current file & directory
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"

# Function to compile data files
compile_dat() {
    local TMPDIR
    TMPDIR=$(mktemp -d)  # Create a temporary directory

    # Error handling trap
    trap 'echo -e "Aborted, error $? in command: $BASH_COMMAND"; rm -rf "$TMPDIR"; exit 1' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

    # Clone or update the geosite repository
    if [[ -d "${GEOSITE}" ]]; then
        (cd "${GEOSITE}" && git pull)
    else
        mkdir -p "${GEOSITE}"
        git clone https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
    fi

    # Run the main.go script to generate dlc.dat
    (cd "${GEOSITE}" && go run main.go)

    # Update geosite.dat if dlc.dat exists
    if [[ -e "${GEOSITE}/dlc.dat" ]]; then
        mv -f "${GEOSITE}/dlc.dat" "$DATADIR/geosite.dat"
        echo "----------> geosite.dat updated."
    else
        echo "----------> geosite.dat failed to update."
    fi

    # Ensure geoip binary is available
    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        go get -v -u github.com/v2ray/geoip || { echo "Failed to install geoip."; exit 1; }
    fi

    # Download and process GeoLite data
    (cd "$TMPDIR" && {
        curl -L -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip || { echo "Failed to download GeoLite2 data."; exit 1; }
        unzip -q GeoLite2-Country-CSV.zip || { echo "Failed to unzip GeoLite2 data."; exit 1; }
        
        mkdir geoip && find . -name '*.csv' -exec mv -t ./geoip {} +

        "$GOPATH/bin/geoip" \
            --country=./geoip/GeoLite2-Country-Locations-en.csv \
            --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
            --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv || { echo "Failed to process GeoLite2 data."; exit 1; }
    })

    # Update geoip.dat if it exists
    if [[ -e geoip.dat ]]; then
        mv -f geoip.dat "$DATADIR/geoip.dat"
        echo "----------> geoip.dat updated."
    else
        echo "----------> geoip.dat failed to update."
    fi

    rm -rf "$TMPDIR"  # Clean up temporary directory
}

# Function to download data files from GitHub releases
download_dat() {
    local geoip_url geosite_url

    geoip_url=$(wget -qO - https://api.github.com/repos/v2ray/geoip/releases/latest | grep browser_download_url | cut -d '"' -f 4)
    
    if ! wget -qO "$DATADIR/geoip.dat" "$geoip_url"; then
        echo "Failed to download geoip.dat from $geoip_url"
        exit 1
    fi

    geosite_url=$(wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest | grep browser_download_url | cut -d '"' -f 4)
    
    if ! wget -qO "$DATADIR/geosite.dat" "$geosite_url"; then
        echo "Failed to download geosite.dat from $geosite_url"
        exit 1
    fi
}

# Determine action based on input argument or default to download
ACTION="${1:-download}"

case $ACTION in
    "download") download_dat ;;
    "compile") compile_dat ;;    
    *) echo "Invalid action. Use 'download' or 'compile'." ; exit 1 ;;
esac

