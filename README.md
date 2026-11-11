# AndroidLibXrayLite

## Build requirements
* JDK
* Android SDK
* Go
* gomobile

## Build instructions
1. `git clone [repo] && cd AndroidLibXrayLite`
2. `gomobile init`
3. `go mod tidy -v`
4. `gomobile bind -v -androidapi 24 -trimpath -ldflags='-s -w -buildid= -checklinkname=0' ./`

## Reproducing a release
Each release publishes a `libv2ray-build.lock` next to `libv2ray.aar`, recording
the inputs its tagged source does not pin by itself: the Go release it was built
with, and the exact geo data embedded in `assets/`. To rebuild a release's aar
byte for byte:

1. Check out the release tag, and use the JDK, NDK and Android SDK versions named in `.github/workflows/main.yml`
2. Install the Go release named by `GO_VERSION` in `libv2ray-build.lock`
3. `mkdir -p assets data && bash gen_assets.sh pinned libv2ray-build.lock && cp data/*.dat assets/`
4. `go install golang.org/x/mobile/cmd/gomobile@$(go list -m -f '{{.Version}}' golang.org/x/mobile)`
5. `gomobile init`, `go mod tidy -v`, then the `gomobile bind` command from step 4 above
