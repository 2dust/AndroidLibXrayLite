//go:build !windows
// +build !windows

package dnscrypt_proxy

import (
	"log"
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
