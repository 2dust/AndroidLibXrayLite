#!/bin/bash

set -o errexit
set -o pipefail
set -o nounset

# Set magic variables for current file & dir
__dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
__file="${__dir}/$(basename "${BASH_SOURCE[0]}")"
__base="$(basename "${__file}" .sh)"

DATADIR="${__dir}/data"

# Ensure the data directory exists
mkdir -p "$DATADIR"

# Function to handle errors
error_exit() {
    echo -e "Aborted, error $? in command: $BASH_COMMAND"
    [[ -d "${TMPDIR:-}" ]] && rm -rf "$TMPDIR"
    exit 1
}

# Function to log messages with timestamps
log() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] $*"
}

# Compile data function
compile_dat() {
    TMPDIR=$(mktemp -d)
    trap 'error_exit' ERR

    local GEOSITE="${GOPATH}/src/github.com/v2ray/domain-list-community"

    log "Checking geosite repository..."
    if [[ -d ${GEOSITE} ]]; then
        (cd "${GEOSITE}" && git pull --quiet)
        log "Geosite repository updated."
    else
        git clone --quiet https://github.com/v2ray/domain-list-community.git "${GEOSITE}"
        log "Geosite repository cloned."
    fi
    
    (cd "${GEOSITE}" && go run main.go)

    if [[ -e "${GEOSITE}/dlc.dat" ]]; then
        mv -f "${GEOSITE}/dlc.dat" "$DATADIR/geosite.dat"
        log "geosite.dat updated successfully."
    else
        log "Failed to update geosite.dat."
    fi

    if [[ ! -x "$GOPATH/bin/geoip" ]]; then
        log "Installing geoip tool..."
        go install github.com/v2ray/geoip@latest
        log "geoip tool installed."
    fi

    cd "$TMPDIR"
    
    log "Downloading GeoLite2 data..."
    curl -sSL -O http://geolite.maxmind.com/download/geoip/database/GeoLite2-Country-CSV.zip
    unzip -q GeoLite2-Country-CSV.zip
    mkdir geoip && mv *.csv geoip/

    "$GOPATH/bin/geoip" \
        --country=./geoip/GeoLite2-Country-Locations-en.csv \
        --ipv4=./geoip/GeoLite2-Country-Blocks-IPv4.csv \
        --ipv6=./geoip/GeoLite2-Country-Blocks-IPv6.csv

    if [[ -e geoip.dat ]]; then
        mv -f geoip.dat "$DATADIR/geoip.dat"
        log "geoip.dat updated successfully."
    else
        log "Failed to update geoip.dat."
    fi
    
    trap - ERR  # Disable error trap
}

# Download data function
download_dat() {
    log "Fetching latest geoip.dat URL..."
    local GEOIP_URL=$(wget -qO - https://api.github.com/repos/dyhkwong/v2ray-geoip/releases/latest | jq -r .assets[].browser_download_url | grep geoip.dat || true)
    
    if [[ -n $GEOIP_URL ]]; then
        wget -qO "$DATADIR/geoip.dat" "$GEOIP_URL"
        log "geoip.dat downloaded successfully."
    else
        log "Failed to fetch geoip.dat URL or download the file."
    fi

    log "Fetching latest geosite.dat URL..."
    local GEOSITE_URL=$(wget -qO - https://api.github.com/repos/v2ray/domain-list-community/releases/latest | grep browser_download_url | cut -d '"' -f 4 || true)
    
    if [[ -n $GEOSITE_URL ]]; then
        wget -qO "$DATADIR/geosite.dat" "$GEOSITE_URL"
        log "geosite.dat downloaded successfully."
    else
        log "Failed to fetch geosite.dat URL or download the file."
    fi
}

# Main execution logic with input validation
ACTION="${1:-download}"

case $ACTION in
    "download") 
        download_dat 
        ;;
        
    "compile") 
        compile_dat 
        ;;
        
    *)
        echo "Invalid action: $ACTION. Use 'download' or 'compile'."
        exit 1
        ;;
esac

