//go:build !freebsd && !openbsd && !windows && !darwin && !linux
// +build !freebsd,!openbsd,!windows,!darwin,!linux

package dnscrypt_proxy

import (
	"net"
)

func (proxy *Proxy) udpListenerConfig() (*net.ListenConfig, error) {
	return &net.ListenConfig{}, nil
}

func (proxy *Proxy) tcpListenerConfig() (*net.ListenConfig, error) {
	return &net.ListenConfig{}, nil
}
