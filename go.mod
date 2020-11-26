module github.com/2dust/AndroidLibV2rayLite

go 1.14

require (
	go.starlark.net v0.0.0-20191021185836-28350e608555 // indirect
	golang.org/x/mobile v0.0.0-20200329125638-4c31acba0007
	golang.org/x/sys v0.0.0-20200323222414-85ca7c5b95cd
	golang.org/x/text v0.3.2 // indirect
	v2ray.com/core v4.19.1+incompatible
)

replace v2ray.com/core => github.com/v2fly/v2ray-core master

