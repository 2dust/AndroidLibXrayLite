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
	lastResolved time.Time
	ipIdx        uint8
	ipLock       sync.Mutex
	lastSwitched time.Time
}

// NextIP switches to another resolved result with throttling.
func (r *resolved) NextIP() {
	r.ipLock.Lock()
	defer r.ipLock.Unlock()

	if len(r.IPs) <= 1 {
		return
	}

	now := time.Now()
	if now.Sub(r.lastSwitched) < 5*time.Second {
		log.Println("Switching IP too quickly")
		return
	}
	r.lastSwitched = now

	r.ipIdx++
	if r.ipIdx >= uint8(len(r.IPs)) {
		r.ipIdx = 0
	}

	log.Printf("Switched to next IP: %v", r.IPs[r.ipIdx])
}

func (r *resolved) currentIP() net.IP {
	r.ipLock.Lock()
	defer r.ipLock.Unlock()
	if len(r.IPs) > 0 {
		return r.IPs[r.ipIdx]
	}
	return nil
}

// NewProtectedDialer initializes a new ProtectedDialer.
func NewProtectedDialer(p protectSet) *ProtectedDialer {
	return &ProtectedDialer{
		resolver:   &net.Resolver{PreferGo: false},
		protectSet: p,
	}
}

// ProtectedDialer manages connections with protection.
type ProtectedDialer struct {
	currentServer string
	resolveChan   chan struct{}
	preferIPv6    bool
	vServer       *resolved
	resolver      *net.Resolver
	protectSet
}

// IsVServerReady checks if the virtual server is ready.
func (d *ProtectedDialer) IsVServerReady() bool {
	return d.vServer != nil
}

// PrepareResolveChan prepares the channel for resolving.
func (d *ProtectedDialer) PrepareResolveChan() {
	d.resolveChan = make(chan struct{})
}

// ResolveChan returns the resolve channel.
func (d *ProtectedDialer) ResolveChan() chan struct{} {
	return d.resolveChan
}

// lookupAddr resolves the address and returns a resolved struct.
func (d *ProtectedDialer) lookupAddr(addr string) (*resolved, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		log.Printf("SplitHostPort error: %v", err)
		return nil, err
	}

	portnum, err := d.resolver.LookupPort(ctx, "tcp", port)
	if err != nil {
		log.Printf("LookupPort error: %v", err)
		return nil, err
	}

	addrs, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil || len(addrs) == 0 {
		return nil, fmt.Errorf("failed to resolve domain %s: %v", addr, err)
	}

	var IPs []net.IP
	for _, ia := range addrs {
		if d.preferIPv6 && ia.IP.To4() == nil || !d.preferIPv6 && ia.IP.To4() != nil {
			IPs = append(IPs, ia.IP)
		}
	}

	rs := &resolved{
		domain:       host,
		IPs:          IPs,
		Port:         portnum,
		lastResolved: time.Now(),
	}
	return rs, nil
}

// PrepareDomain caches the direct V2Ray server host.
func (d *ProtectedDialer) PrepareDomain(domainName string, closeCh <-chan struct{}, prefIPv6 bool) {
	log.Printf("Preparing Domain: %s", domainName)
	d.currentServer = domainName
	d.preferIPv6 = prefIPv6

	maxRetry := 10
	for maxRetry > 0 {
		resolved, err := d.lookupAddr(domainName)
		if err != nil {
			maxRetry--
			log.Printf("PrepareDomain error: %v", err)

			select {
			case <-closeCh:
				log.Println("PrepareDomain exited due to closure")
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		d.vServer = resolved
		log.Printf("Prepare Result:\n Domain: %s\n Port: %d\n IPs: %v\n",
			resolved.domain, resolved.Port, resolved.IPs)
		return
	}
	log.Println("PrepareDomain maxRetry reached. Exiting.")
}

// getFd retrieves a file descriptor for the specified network type.
func (d *ProtectedDialer) getFd(network v2net.Network) (int, error) {
	switch network {
	case v2net.Network_TCP:
		return unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	case v2net.Network_UDP:
		return unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	default:
		return -1, fmt.Errorf("unknown network type")
	}
}

// Init implements internet.SystemDialer.
func (d *ProtectedDialer) Init(_ dns.Client, _ outbound.Manager) {}

// Dial establishes a protected connection to the destination.
func (d *ProtectedDialer) Dial(ctx context.Context,
	src v2net.Address, dest v2net.Destination, sockopt *v2internet.SocketConfig) (net.Conn, error) {

	address := dest.NetAddr()

	if address == d.currentServer {
		if d.vServer == nil {
			log.Println("Dial pending preparation...", address)
			select {
			case <-d.resolveChan:
				if d.vServer == nil {
					return nil, fmt.Errorf("failed to prepare domain %s", d.currentServer)
				}
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				return nil, errors.New("preparation in progress")
			}
		}

        fd, err := d.getFd(dest.Network)
        if err != nil {
            return nil, err
        }

        curIP := d.vServer.currentIP()
        conn, err := d.fdConn(ctx, curIP, d.vServer.Port, dest.Network, fd)
        if err != nil {
            d.vServer.NextIP()
            return nil, err
        }
        log.Printf("Using Prepared IP: %s", curIP)
        return conn, nil
    }

    log.Printf("Not Using Prepared: %s,%s", dest.Network.SystemString(), address)
    resolved, err := d.lookupAddr(address)
    if err != nil {
        return nil, err
    }

    fd, err := d.getFd(dest.Network)
    if err != nil {
        return nil, err
    }

    return d.fdConn(ctx, resolved.IPs[0], resolved.Port, dest.Network, fd)
}

// DestIpAddress returns the current destination IP address.
func (d *ProtectedDialer) DestIpAddress() net.IP {
    return d.vServer.currentIP()
}

// fdConn establishes a connection using the provided file descriptor.
func (d *ProtectedDialer) fdConn(ctx context.Context, ip net.IP, port int,
    network v2net.Network, fd int) (net.Conn, error) {

	defer unix.Close(fd)

	if !d.Protect(fd) {
	    log.Printf("fdConn failed to protect FD: %d", fd)
	    return nil, errors.New("failed to protect")
    }

	sa := &unix.SockaddrInet6{Port: port}
	copy(sa.Addr[:], ip.To16())

	var bindErr error

	if network == v2net.Network_UDP {
	    bindErr = unix.Bind(fd, &unix.SockaddrInet6{})
    } else {
	    bindErr = unix.Connect(fd, sa)
    }

	if bindErr != nil {
	    log.Printf("fdConn bind/connect error on FD: %d Err: %v", fd, bindErr)
	    return nil, bindErr
    }

	file := os.NewFile(uintptr(fd), "Socket")
	if file == nil {
	    return nil, errors.New("invalid file descriptor")
    }
	defer file.Close()

	if network == v2net.Network_UDP {
	    packetConn, err := net.FilePacketConn(file)
	    if err != nil {
	        log.Printf("fdConn FilePacketConn error on FD: %d Err: %v", fd, err)
	        return nil, err
	    }
	    return &v2internet.PacketConnWrapper{
	        Conn: packetConn,
	        Dest: &net.UDPAddr{IP: ip, Port: port},
	    }, nil
    } else {
	    conn, err := net.FileConn(file)
	    if err != nil {
	        log.Printf("fdConn FileConn error on FD: %d Err: %v", fd, err)
	        return nil, err
	    }
	    return conn, nil
    }
}
