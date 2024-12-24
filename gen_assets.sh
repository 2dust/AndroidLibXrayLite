#!/bin/bash

set -euo pipefail

# Create necessary directories
mkdir -p assets data

# Generate assets
echo "Generating assets..."
bash gen_assets.sh download

# Copy .dat files to assets directory
echo "Copying data files to assets..."
cp -v data/*.dat assets/

# Initialize Go Mobile environment
echo "Initializing Go Mobile..."
gomobile init

# Clean up Go module dependencies
echo "Tidying up Go modules..."
go mod tidy

# Bind for Android with specified parameters
echo "Binding Go package for Android..."
gomobile bind -v -androidapi 21 -ldflags='-s -w' ./

echo "Build process completed successfully."
