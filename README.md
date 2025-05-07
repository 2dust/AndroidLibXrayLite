# AndroidLibXrayLite

## Build requirements
* JDK
* Android SDK
* Gob
* gomobile

## Build instructions
1. `git clone [repo] && cd AndroidLibXrayLite`
2. `gomobile init`
3. `go mod tidy -v`
4. `gomobile bind -v -androidapi 21 -ldflags='-s -w' ./`
