package libv2ray

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	v2net "github.com/xtls/xray-core/common/net"
)

type fakeSupportSet struct{}

func (f fakeSupportSet) Protect(int) bool {
	return true
}

func setupDialer() *ProtectedDialer {
	return NewPreotectedDialer(fakeSupportSet{})
}

func TestProtectedDialer_PrepareDomain(t *testing.T) {
	tests := []struct {
		name       string
		domainName string
	}{
		{"baidu.com:80", "baidu.com:80"},
		// {"cloudflare.com:443", "cloudflare.com:443"},
	}

	d := setupDialer()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan struct{})
			go d.PrepareDomain(tt.domainName, ch, false)

			time.Sleep(time.Second)
			go d.vServer.NextIP()
			t.Log(d.vServer.currentIP())
		})
	}

	time.Sleep(time.Second)
}

func TestProtectedDialer_Dial(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"baidu.com:80", false},
		{"cloudflare.com:80", false},
		{"172.16.192.11:80", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := setupDialer()
			d.currentServer = tt.name
			ch := make(chan struct{})
			go d.PrepareDomain(tt.name, ch, false)

			var wg sync.WaitGroup
			dial := func() {
				defer wg.Done()
				dest, _ := v2net.ParseDestination("tcp:" + tt.name)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				conn, err := d.Dial(ctx, nil, dest, nil)
				if (err != nil) != tt.wantErr {
					t.Errorf("Dial() error = %v, wantErr %v", err, tt.wantErr)
					return
				}
				if conn != nil {
					defer conn.Close()
					host, _, _ := net.SplitHostPort(tt.name)
					fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
					status, err := bufio.NewReader(conn).ReadString('\n')
					t.Logf("Response status: %#v, error: %#v\n", status, err)
				}
			}

			for n := 0; n < 3; n++ {
				wg.Add(1)
				go dial()
			}

			wg.Wait()
		})
	}
}

func Test_resolved_NextIP(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		IPs    []net.IP
	}{
		{"test1", "www.baidu.com", []net.IP{
			net.ParseIP("1.2.3.4"),
			net.ParseIP("4.3.2.1"),
			net.ParseIP("1234::1"),
			net.ParseIP("4321::1"),
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &resolved{
				domain: tt.domain,
				IPs:    tt.IPs,
			}
			for i := 0; i < len(r.IPs)+1; i++ { // Loop through all IPs and one extra for wrap-around
				t.Logf("Current IP before NextIP: %v", r.currentIP())
				r.NextIP()
				t.Logf("Current IP after NextIP: %v", r.currentIP())
				time.Sleep(1 * time.Second) // Adjust sleep as needed for testing
			}
		})
	}
}
