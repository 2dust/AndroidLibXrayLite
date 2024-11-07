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

// NextIP switches to another resolved result.
// There may still be a race condition here if multiple errors occur concurrently.
// This can cause idx to keep switching, but that's an outside error that is hard to handle here.
func (r *resolved) NextIP() {
	r.ipLock.Lock()
	defer r.ipLock.Unlock()

	if len(r.IPs) > 1 {
		// Throttle, don't switch too quickly
		now := time.Now()
		if now.Sub(r.lastSwitched) < time.Second*5 {
			log.Println("switch too quickly")
			return
		}
		r.lastSwitched = now
		r.ipIdx++
	} else {
		return
	}

	if r.ipIdx >= uint8(len(r.IPs)) {
		r.ipIdx = 0
	}
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

// NewProtectedDialer creates a new ProtectedDialer.
func NewProtectedDialer(p protectSet) *ProtectedDialer {
	d := &ProtectedDialer{
		resolver:   &net.Resolver{PreferGo: false}, // prefer native lookup on Android resolver
		protectSet: p,
	}
	return d
}

// ProtectedDialer ...
type ProtectedDialer struct {
	currentServer string
	resolveChan   chan struct{}
	preferIPv6    bool
	vServer       *resolved
	resolver      *net.Resolver
	protectSet    protectSet
}

func (d *ProtectedDialer) IsVServerReady() bool {
	return (d.vServer != nil)
}

func (d *ProtectedDialer) PrepareResolveChan() {
	d.resolveChan = make(chan struct{})
}

func (d *ProtectedDialer) ResolveChan() chan struct{} {
	return d.resolveChan
}

// Simplified version of golang: internetAddrList in src/net/ipsock.go
func (d *ProtectedDialer) lookupAddr(addr string) (*resolved, error) {
	var (
		err      error
		host     string
		port     string
		portnum  int
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if host, port, err = net.SplitHostPort(addr); err != nil {
		log.Printf("PrepareDomain SplitHostPort Err: %v", err)
		return nil, err
	}

	if portnum, err = d.resolver.LookupPort(ctx, "tcp", port); err != nil {
		log.Printf("PrepareDomain LookupPort Err: %v", err)
		return nil, err
	}

	addrs, err := d.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("domain %s failed to resolve", addr)
	}

	var IPs []net.IP

	if d.preferIPv6 {
		for _, ia := range addrs {
			if ia.IP.To4() == nil { // IPv6 address
			 IPs = append(IPs, ia.IP)
		 }
	  }
   }

	for _, ia := range addrs {
	  if ia.IP.To4() != nil { // IPv4 address
	    IPs = append(IPs, ia.IP)
	  }
   }

	if !d.preferIPv6 { // If not prefer IPv6, append IPv4 first.
	  for _, ia := range addrs {
	    if ia.IP.To4() == nil { // IPv6 address
	      IPs = append(IPs, ia.IP)
	    }
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

// PrepareDomain caches direct v2ray server host.
func (d *ProtectedDialer) PrepareDomain(domainName string, closeCh <-chan struct{}, prefIPv6 bool) {
	log.Printf("Preparing Domain: %s", domainName)
	d.currentServer = domainName
	d.preferIPv6 = prefIPv6

	maxRetry := 10

	for {
	  if maxRetry == 0 {
	    log.Println("PrepareDomain maxRetry reached. exiting.")
	    return 
	  }

	  resolved, err := d.lookupAddr(domainName)
	  if err != nil {
	    maxRetry--
	    log.Printf("PrepareDomain err: %v\n", err)

	    select {
	      case <-closeCh:
	        log.Printf("PrepareDomain exit due to core closed")
	        return 
	      case <-time.After(time.Second * 2):
	    }
	    continue 
	  }

	  d.vServer = resolved 
	  log.Printf("Prepare Result:\n Domain: %s\n Port: %d\n IPs: %v\n", resolved.domain, resolved.Port, resolved.IPs)
	  return 
   }
}

func (d *ProtectedDialer) getFd(network v2net.Network) (fd int, err error) {
	switch network {
	case v2net.Network_TCP:
	    fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	case v2net.Network_UDP:
	    fd, err = unix.Socket(unix.AF_INET6, unix.SOCK_DGRAM, unix.IPPROTO_UDP)
	default:
	    err = fmt.Errorf("unknown network")
    }
    return 
}

// Init implements internet.SystemDialer.
func (d *ProtectedDialer) Init(_ dns.Client, _ outbound.Manager) { 
    // do nothing 
}

// Dial exported as the protected dial method.
func (d *ProtectedDialer) Dial(ctx context.Context, src v2net.Address, dest v2net.Destination, sockopt *v2internet.SocketConfig) (net.Conn, error) {
    network := dest.Network.SystemString()
    Address := dest.NetAddr()

    // v2ray server address,
    // try to connect fixed IP if multiple IP parsed from domain,
    // and switch to next IP if error occurred.
    if Address == d.currentServer {
        if d.vServer == nil { 
            log.Println("Dial pending prepare ...", Address)
            <-d.resolveChan 

            // User may close connection during PrepareDomain,
            // fast return release resources.
            if d.vServer == nil { 
                return nil, fmt.Errorf("fail to prepare domain %s", d.currentServer)
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

        log.Printf("Using Prepared: %s", curIP)
        return conn,nil 
    }

    // V2Ray connecting to "domestic" servers; no caching results.
    log.Printf("Not Using Prepared: %s,%s", network , Address)

    resolved ,err:= d.lookupAddr(Address)
    if err !=nil{
      return nil ,err 
    }

    fd ,err:= d.getFd(dest.Network )
    if err!=nil{
      return nil ,err 
    }

    // Use the first resolved address.
    // The result IP may vary; e.g., IPv6 addresses come first if client has an IPv6 address.
    return d.fdConn(ctx ,resolved.IPs[0], resolved.Port ,dest.Network ,fd )
}

func (d *ProtectedDialer) DestIpAddress() net.IP { 
    return d.vServer.currentIP() 
}

func (d *ProtectedDialer) fdConn(ctx context.Context ,ip net.IP ,port int ,network v2net.Network ,fd int ) (net.Conn ,error ) { 
    defer unix.Close(fd)

    // Call Android VPN service to "protect" the fd connecting straight out.
    if !d.Protect(fd ) { 
        log.Printf("fdConn fail to protect; Close Fd: %d", fd )
        return nil ,errors.New("fail to protect") 
     } 

     sa := &unix.SockaddrInet6{ Port : port , } 
     copy(sa.Addr[:], ip.To16()) 

     if network == v2net.Network_UDP { 
         if err := unix.Bind(fd ,&unix.SockaddrInet6{} );err!=nil{ 
             log.Printf("fdConn unix.Bind error; Close Fd: %d Err: %v", fd ,err ) 
             return nil ,err 
         } 
     } else { 
         if err := unix.Connect(fd ,sa );err!=nil{ 
             log.Printf("fdConn unix.Connect error; Close Fd: %d Err: %v", fd ,err ) 
             return nil ,err 
         } 
     } 

     file := os.NewFile(uintptr(fd), "Socket") 

     if file ==nil{ // Returned value will be nil if fd is not a valid file descriptor. 
         return nil ,errors.New("fdConn fd invalid")  
     } 

     defer file.Close() // Closing conn does not affect file and closing file does not affect conn.

     if network == v2net.Network_UDP { 
         packetConn ,err:= net.FilePacketConn(file ) 

         if err!=nil{ 
             log.Printf("fdConn FilePacketConn Close Fd: %d Err: %v", fd ,err )  
             return nil ,err  
         } 

         return &v2internet.PacketConnWrapper{ Conn : packetConn , Dest : &net.UDPAddr{ IP : ip , Port : port , }, },nil  
     } else { 
         conn ,err:= net.FileConn(file )  
         if err!=nil{  
             log.Printf("fdConn FileConn Close Fd: %d Err: %v", fd ,err )  
             return nil ,err  
         }  

         return conn,nil  
     }  
}
