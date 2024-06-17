//go:build !linux && !windows
// +build !linux,!windows

package dnscrypt_proxy

func ServiceManagerStartNotify() error {
	return nil
}

func ServiceManagerReadyNotify() error {
	return nil
}
