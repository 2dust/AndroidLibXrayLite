package NebulaVPNComponents

import (
	"log"
	"sync"

	"gitlab.com/nebulavpn/dnscrypt_proxy/dnscrypt_lib"
)

var quit chan struct{}
var ps *dnscrypt_lib.ProxyServer

func DnscryptIsRunning() bool {
	return ps != nil && ps.HasListener()
}

func DnscryptStart(config string) (int, error) {
	if DnscryptIsRunning() {
		return 1, nil
	}

	var wg sync.WaitGroup

	quit = make(chan struct{})
	wg.Add(1)

	ps, err := dnscrypt_lib.Start(config)
	if err != nil {
		return 2, err
	}
	defer ps.Stop()

	<-quit
	log.Println("Quit signal received...")
	wg.Done()
	ps.Stop()
	return 0, nil
}

func DnscryptStop() {
	if DnscryptIsRunning() {
		ps.Stop()
	}
}
