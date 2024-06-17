//go:build !linux
// +build !linux

package dnscrypt_proxy

func (proxy *Proxy) addSystemDListeners() error {
	return nil
}
