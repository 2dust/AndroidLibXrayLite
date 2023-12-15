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
4. `gomobile bind -v -androidapi 19 -ldflags='-s -w' ./`

## People who can't compile themselves
- They can download the `libv2ray.aar` and the `libv2ray-sources.jar` directly from [here](https://github.com/omid-the-great/AndroidLibXrayLite/releases)
