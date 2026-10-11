package libv2ray

import (
	"net/http"
	"testing"
)

func TestApplyRequestHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}

	applyRequestHeaders(req, map[string]string{
		"host":   "origin.example",
		"X-Test": "value",
	})

	if req.Host != "origin.example" {
		t.Fatalf("Host = %q, want %q", req.Host, "origin.example")
	}
	if got := req.Header.Get("Host"); got != "" {
		t.Fatalf("Header[Host] = %q, want empty", got)
	}
	if got := req.Header.Get("X-Test"); got != "value" {
		t.Fatalf("X-Test = %q, want %q", got, "value")
	}
}
