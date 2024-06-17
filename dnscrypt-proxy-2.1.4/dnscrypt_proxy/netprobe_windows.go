package dnscrypt_proxy

import (
	"net"
	"time"
)

func NetProbe(proxy *Proxy, address string, timeout int) error {
	if len(address) <= 0 || timeout == 0 {
		return nil
	}
	if captivePortalHandler, err := ColdStart(proxy); err == nil {
		if captivePortalHandler != nil {
			defer captivePortalHandler.Stop()
		}
	} else {
		log.Println(err)
	}
	remoteUDPAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return err
	}
	retried := false
	if timeout < 0 {
		timeout = MaxTimeout
	} else {
		timeout = Min(MaxTimeout, timeout)
	}
	for tries := timeout; tries > 0; tries-- {
		pc, err := net.DialUDP("udp", nil, remoteUDPAddr)
		if err == nil {
			// Write at least 1 byte. This ensures that sockets are ready to use for writing.
			// Windows specific: during the system startup, sockets can be created but the underlying buffers may not be
			// setup yet. If this is the case Write fails with WSAENOBUFS: "An operation on a socket could not be
			// performed because the system lacked sufficient buffer space or because a queue was full"
			_, err = pc.Write([]byte{0})
		}
		if err != nil {
			if !retried {
				retried = true
				log.Println("Network not available yet -- waiting...")
			}
			log.Println(err)
			time.Sleep(1 * time.Second)
			continue
		}
		pc.Close()
		log.Println("Network connectivity detected")
		return nil
	}
	log.Println("Timeout while waiting for network connectivity")
	return nil
}
