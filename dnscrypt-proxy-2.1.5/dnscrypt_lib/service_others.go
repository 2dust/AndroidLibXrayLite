//go:build !linux && !windows
// +build !linux,!windows

package dnscrypt_lib

func ServiceManagerStartNotify() error {
	return nil
}

func ServiceManagerReadyNotify() error {
	return nil
}
