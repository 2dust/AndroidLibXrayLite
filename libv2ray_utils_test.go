package libv2ray

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestLegacyOutboundDelaySupportsGetOnlyEndpoints(t *testing.T) {
	var getRequests, headRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			headRequests.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		getRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	config := `{"log":{"loglevel":"none"},"outbounds":[{"protocol":"freedom","tag":"proxy"}]}`
	delay, err := MeasureOutboundDelay(config, server.URL)
	if err != nil || delay < 0 {
		t.Fatalf("working GET endpoint rejected: delay=%d error=%v", delay, err)
	}
	if getRequests.Load() != 2 || headRequests.Load() != 0 {
		t.Fatalf("legacy request policy changed: GET=%d HEAD=%d", getRequests.Load(), headRequests.Load())
	}
}

func TestProbeFallbackRetainsSingleHeadRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	controller := NewProbeController()
	defer controller.Cancel()
	config := `{"log":{"loglevel":"none"},"outbounds":[{"protocol":"freedom","tag":"proxy"}]}`
	delay, err := controller.MeasureDelay(config, server.URL)
	if err != nil || delay < 0 {
		t.Fatalf("probe fallback failed: delay=%d error=%v", delay, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("probe fallback made %d requests, want 1", requests.Load())
	}
}
