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

// Other methods of ProtectedDialer...
