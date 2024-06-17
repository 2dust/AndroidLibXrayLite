package dnscrypt_lib

import (
	crypto_rand "crypto/rand"
	"encoding/binary"
	"math/rand"
	"net"
	"runtime"

	"gitlab.com/nebulavpn/dnscrypt_proxy/dnscrypt_proxy"
)

type ProxyServer struct {
	U *net.UDPConn
	T *net.TCPListener
	D *net.TCPListener
}

func (ps *ProxyServer) Stop() {
	if ps.U != nil {
		ps.U.Close()
	}
	if ps.T != nil {
		ps.T.Close()
	}
	if ps.D != nil {
		ps.D.Close()
	}
	ps.U = nil
	ps.T = nil
	ps.D = nil
}

func (ps *ProxyServer) HasListener() bool {
	return ps.U != nil || ps.T != nil || ps.D != nil
}

func Start(config string) (*ProxyServer, error) {
	dnscrypt_proxy.TimezoneSetup()
	runtime.MemProfileRate = 0
	seed := make([]byte, 8)
	crypto_rand.Read(seed)
	rand.Seed(int64(binary.LittleEndian.Uint64(seed[:])))

	var proxy *dnscrypt_proxy.Proxy = dnscrypt_proxy.NewProxy()
    configFlags := ConfigFlags{
          ConfigFile: config
       }
	if err := dnscrypt_proxy.ConfigLoad(proxy, configFlags); err != nil {
		return nil, err
	}
	// if err := dnscrypt_proxy.PidFileCreate(); err != nil {
	// 	log.Printf("Unable to create the PID file: %v", err)
	// }
	if err := proxy.InitPluginsGlobals(); err != nil {
		return nil, err
	}

	ps := &ProxyServer{}
	ps.U, ps.T, ps.D = proxy.StartProxy()
	runtime.GC()
	return ps, nil
}
