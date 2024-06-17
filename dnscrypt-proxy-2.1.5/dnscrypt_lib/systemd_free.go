//go:build !linux
// +build !linux

package dnscrypt_lib

func (proxy *Proxy) addSystemDListeners() error {
	return nil
}
