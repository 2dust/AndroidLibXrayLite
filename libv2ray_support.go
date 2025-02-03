package libv2ray

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	v2net "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	v2internet "github.com/xtls/xray-core/transport/internet"
)

type protectSet interface {
	Protect(int) bool
}

type resolved struct {
	domain       string
	IPs          []net.IP
	Port         int
	ipIdx        uint8
	ipLock       sync.Mutex
	lastSwitched time.Time
}

func (r *resolved) NextIP() {
	r.ipLock.Lock()
	defer r.ipLock.Unlock()

	if len(r.IPs) <= 1 {
		return
	}

	if time.Since(r.lastSwitched) < 5*time.Second {
		log.Println("switch too quickly")
		return
	}

	r.ipIdx = (r.ipIdx + 1) % uint8(len(r.IPs))
	r.lastSwitched = time.Now()
	log.Printf("switched to next IP: %v", r.IPs[r.ipIdx])
}

func (r *resolved) currentIP() net.IP {
	r.ipLock.Lock()
	defer r.ipLock.Unlock()
	if len(r.IPs) > 0 {
		return r.IPs[r.ipIdx]
	}
	return nil
}

func NewProtectedDialer(p protectSet) *ProtectedDialer {
	return &ProtectedDialer{
		resolver:   &net.Resolver{PreferGo: false},
		protectSet: p,
	}
}

type ProtectedDialer struct {
	currentServer string
	resolveChan   chan struct{}
	preferIPv6    bool
	vServer       *resolved
	resolver      *net.Resolver
	protectSet
}

func (d *ProtectedDialer) PrepareResolveChan() {
	d.resolveChan = make(chan struct{})
}

func (d *ProtectedDialer) lookupAddr(addr string) (*resolved, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, logError("SplitHostPort", err)
	}

	portnum, err := d.resolver.LookupPort(ctx, "tcp", port)
	if err != nil {
		return nil, logError("LookupPort", err)
	}

	addrs, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("domain %s failed to resolve", addr)
	}

	var IPs []net.IP
	for _, ia := range addrs {
		if (d.preferIPv6 && ia.IP.To4() == nil) || (!d.preferIPv6 && ia.IP.To4() != nil) {
			IPs = append(IPs, ia.IP)
		}
	}

	return &resolved{
		domain:       host,
		IPs:          IPs,
		Port:         portnum,
	}, nil
}

func logError(action string, err error) error {
	log.Printf("%s error: %v", action, err)
	return err
}

// PrepareDomain caches direct v2ray server host
func (d *ProtectedDialer) PrepareDomain(domainName string, closeCh <-chan struct{}, prefIPv6 bool) {
	log.Printf("Preparing Domain: %s", domainName)
	d.currentServer = domainName
	d.preferIPv6 = prefIPv6

	for retries := 10; retries > 0; retries-- {
		resolved, err := d.lookupAddr(domainName)
		if err != nil {
			log.Printf("PrepareDomain error: %v\n", err)
			select {
			case <-closeCh:
				log.Printf("PrepareDomain exit due to core closed")
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		
		d.vServer = resolved
		log.Printf("Prepared Domain: %s Port: %d IPs: %v\n", resolved.domain, resolved.Port, resolved.IPs)
		return
	}
	log.Println("PrepareDomain max retries reached. Exiting.")
}

// Dial establishes a connection to the V2Ray server.
func (d *ProtectedDialer) Dial(ctx context.Context, src v2net.Address, dest v2net.Destination, sockopt *v2internet.SocketConfig) (net.Conn, error) {
	if d.vServer == nil && d.currentServer == dest.NetAddr() {
        log.Println("Dial pending prepare ...")
        <-d.resolveChan // Wait for resolution to complete.
    }

	curIP := d.vServer.currentIP()
	fd, err := d.getFd(dest.Network)
	if err != nil {
        return nil, err
    }

	conn, err := d.fdConn(ctx, curIP, d.vServer.Port, dest.Network, fd)
	if err != nil {
        d.vServer.NextIP() // Try the next IP if the current one fails.
        return nil, err
    }
    
    log.Printf("Using Prepared IP: %s", curIP)
    return conn, nil
}

// Additional methods (getFd and fdConn) remain unchanged...

