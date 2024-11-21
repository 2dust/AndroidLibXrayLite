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

// TestProtectedDialer_PrepareDomain tests the PrepareDomain method of ProtectedDialer.
func TestProtectedDialer_PrepareDomain(t *testing.T) {
	type args struct {
		domainName string
	}

	tests := []struct {
		name string
		args args
	}{
		// TODO: Add test cases.
		{"Test with baidu.com", args{"baidu.com:80"}},
		// {"Test with cloudflare.com", args{"cloudflare.com:443"}},
		// {"Test with apple.com", args{"apple.com:443"}},
		// {"Test with IPv4", args{"110.110.110.110:443"}},
		// {"Test with IPv6", args{"[2002:1234::1]:443"}},
	}

	d := NewProtectedDialer(fakeSupportSet{})
	for _, tt := range tests {
		ch := make(chan struct{})
		t.Run(tt.name, func(t *testing.T) {
			go d.PrepareDomain(tt.args.domainName, ch, false)
			time.Sleep(time.Second)
			go d.vServer.NextIP()
			t.Log(d.vServer.currentIP())
		})
	}
	time.Sleep(time.Second)
}

// TestProtectedDialer_Dial tests the Dial method of ProtectedDialer.
func TestProtectedDialer_Dial(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		// TODO: Add test cases.
		{"baidu.com:80", false},
		{"cloudflare.com:80", false},
		{"172.16.192.11:80", true},
		// {"172.16.192.10:80", true},
		// {"[2fff:4322::1]:443", true},
		// {"[fc00::1]:443", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan struct{})
			d := NewProtectedDialer(fakeSupportSet{})
			d.currentServer = tt.name

			go d.PrepareDomain(tt.name, ch, false)

			var wg sync.WaitGroup
			dial := func() {
				defer wg.Done()
				dest, _ := v2net.ParseDestination("tcp:" + tt.name)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				conn, err := d.Dial(ctx, nil, dest, nil)
				if err != nil {
					t.Log(err)
					return
				}

				host, _, _ := net.SplitHostPort(tt.name)
				fmt.Fprintf(conn, fmt.Sprintf("GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host))
				status, err := bufio.NewReader(conn).ReadString('\n')
				t.Logf("%#v, %#v\n", status, err)
				conn.Close()
			}

			for n := 0; n < 3; n++ {
				wg.Add(1)
				go dial()
				time.Sleep(time.Millisecond * 10)
			}
			wg.Wait()
		})
	}
}

// Test_resolved_NextIP tests the NextIP method of resolved.
func Test_resolved_NextIP(t *testing.T) {
	type fields struct {
	    domain string
	    IPs    []net.IP
	    Port   int
    }

	tests := []struct {
	    name   string
	    fields fields
    }{
        // TODO: Add test cases.
        {"test1", fields{
            domain: "www.baidu.com",
            IPs: []net.IP{
                net.ParseIP("1.2.3.4"),
                net.ParseIP("4.3.2.1"),
                net.ParseIP("1234::1"),
                net.ParseIP("4321::1"),
            },
        }},
    }

	for _, tt := range tests {
	    t.Run(tt.name, func(t *testing.T) {
	        r := &resolved{
	            domain: tt.fields.domain,
	            IPs:    tt.fields.IPs,
	            Port:   tt.fields.Port,
	        }
	        t.Logf("%v", r.IPs)
	        t.Logf("%v", r.currentIP())
	        r.NextIP()
	        t.Logf("%v", r.currentIP())
	        r.NextIP()
	        t.Logf("%v", r.currentIP())
	        r.NextIP()
	        t.Logf("%v", r.currentIP())
	        
	        time.Sleep(3 * time.Second)
	        r.NextIP()
	        t.Logf("%v", r.currentIP())
	        
	        time.Sleep(5 * time.Second)
	        r.NextIP()
	        t.Logf("%v", r.currentIP())
	    })
    }
}
