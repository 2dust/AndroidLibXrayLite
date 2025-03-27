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

// fakeSupportSet is a mock implementation of the protectSet interface
type fakeSupportSet struct{}

// Protect is a mock implementation that always returns true
func (f fakeSupportSet) Protect(int) bool {
	return true
}

// TestProtectedDialer_PrepareDomain tests the PrepareDomain method of the ProtectedDialer
func TestProtectedDialer_PrepareDomain(t *testing.T) {
	type args struct {
		domainName string
	}
	tests := []struct {
		name string
		args args
	}{
		{"Test with baidu.com", args{"baidu.com:80"}},
		// Add more test cases if needed
	}
	d := NewProtectedDialer(fakeSupportSet{})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan struct{})
			go d.PrepareDomain(tt.args.domainName, ch, false)

			time.Sleep(time.Second)
			d.vServer.NextIP()
			t.Log(d.vServer.currentIP())
		})
	}

	time.Sleep(time.Second)
}

// TestProtectedDialer_Dial tests the Dial method of the ProtectedDialer
func TestProtectedDialer_Dial(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{"baidu.com:80", false},
		{"cloudflare.com:80", false},
		{"172.16.192.11:80", true},
		// Add more test cases if needed
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
				defer conn.Close()

				host, _, _ := net.SplitHostPort(tt.name)
				fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
				status, err := bufio.NewReader(conn).ReadString('\n')
				t.Logf("Status: %s, Error: %v", status, err)
			}

			for n := 0; n < 3; n++ {
				wg.Add(1)
				go dial()
			}

			wg.Wait()
		})
	}
}

// Test_resolved_NextIP tests the NextIP method of the resolved struct
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
			t.Logf("Initial IPs: %v", r.IPs)
			t.Logf("Current IP: %v", r.currentIP())
			r.NextIP()
			t.Logf("Next IP: %v", r.currentIP())
			r.NextIP()
			t.Logf("Next IP: %v", r.currentIP())
			r.NextIP()
			t.Logf("Next IP: %v", r.currentIP())
			time.Sleep(3 * time.Second)
			r.NextIP()
			t.Logf("Next IP: %v", r.currentIP())
			time.Sleep(5 * time.Second)
			r.NextIP()
			t.Logf("Next IP: %v", r.currentIP())
		})
	}
}
