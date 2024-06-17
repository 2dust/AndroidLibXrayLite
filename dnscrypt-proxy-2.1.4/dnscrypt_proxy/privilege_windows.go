package dnscrypt_proxy

import "os"

func (proxy *Proxy) dropPrivilege(userStr string, fds []*os.File) {}
