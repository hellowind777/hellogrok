package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

const breakerProbeBody = `{"model":"display","input":"hi","max_output_tokens":64,"stream":false}`

func upstream502Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "error code: 502")
	}
}

func upstreamOKHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, nativeSuccessBody("responses", "wire", "OK"))
	}
}

// newBreakerTestServer starts a facade for a single channel whose upstream is
// the counting wrapper around handler. The returned counter reports how many
// requests actually reached the upstream.
func newBreakerTestServer(t *testing.T, handler http.Handler) (*Server, *int64) {
	t.Helper()
	var calls int64
	counting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		handler.ServeHTTP(w, r)
	})
	upstream := httptest.NewServer(counting)
	t.Cleanup(upstream.Close)
	route := facadeRoute("breaker-channel", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	return server, &calls
}

func TestBreakerOpensAfterThresholdRetryableFailures(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstream502Handler())

	for i := 0; i < circuitOpenThreshold; i++ {
		_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		if status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status=%d, want 502", i+1, status)
		}
		if header.Get("X-Should-Retry") != "true" {
			t.Fatalf("attempt %d: retryable upstream failure must stay retryable", i+1)
		}
	}
	if got := atomic.LoadInt64(calls); got != circuitOpenThreshold {
		t.Fatalf("upstream calls=%d, want %d", got, circuitOpenThreshold)
	}

	data, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("fast-fail status=%d, want 503", status)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("fast-fail must be non-retryable, got %q", header.Get("X-Should-Retry"))
	}
	if !containsJSONType(data, "proxy_circuit_open") {
		t.Fatalf("fast-fail body missing proxy_circuit_open: %s", data)
	}
	if got := atomic.LoadInt64(calls); got != circuitOpenThreshold {
		t.Fatalf("open breaker must not hit upstream: calls=%d", got)
	}
}

func TestBreakerSuccessResetsStreak(t *testing.T) {
	var upstreamCalls int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&upstreamCalls, 1)%4 == 0 {
			upstreamOKHandler().ServeHTTP(w, r)
			return
		}
		upstream502Handler().ServeHTTP(w, r)
	})
	server, calls := newBreakerTestServer(t, handler)

	for i := 0; i < 12; i++ {
		_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		want := http.StatusBadGateway
		if (i+1)%4 == 0 {
			want = http.StatusOK
		}
		if status != want {
			t.Fatalf("attempt %d: status=%d, want %d", i+1, status, want)
		}
	}
	if got := atomic.LoadInt64(calls); got != 12 {
		t.Fatalf("breaker must never fast-fail a recovering channel: calls=%d", got)
	}
}

func TestBreakerProbeRecoversAfterCooldown(t *testing.T) {
	fail := atomic.Bool{}
	fail.Store(true)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			upstream502Handler().ServeHTTP(w, r)
			return
		}
		upstreamOKHandler().ServeHTTP(w, r)
	})
	server, _ := newBreakerTestServer(t, handler)
	clock := time.Now()
	server.breakers.now = func() time.Time { return clock }

	for i := 0; i < circuitOpenThreshold; i++ {
		postFacade(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	}

	// Within the cooldown the breaker keeps fast-failing.
	_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("in-cooldown status=%d, want 503", status)
	}

	// After the cooldown the first request becomes a probe; it still fails,
	// so the cooldown re-arms and the next request fast-fails again.
	clock = clock.Add(circuitOpenCooldown + time.Second)
	_, status, _ = postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusBadGateway {
		t.Fatalf("failed probe status=%d, want 502", status)
	}
	_, status, _ = postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("re-armed fast-fail status=%d, want 503", status)
	}

	// Upstream recovers; the next probe succeeds and closes the breaker.
	clock = clock.Add(circuitOpenCooldown + time.Second)
	fail.Store(false)
	_, status, _ = postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusOK {
		t.Fatalf("recovering probe status=%d, want 200", status)
	}
	_, status, _ = postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusOK {
		t.Fatalf("post-recovery status=%d, want 200", status)
	}
}

func TestBreakerPerChannelIsolation(t *testing.T) {
	var secondCalls int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&secondCalls, 1)
		upstream502Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(second.Close)

	server, _ := newBreakerTestServer(t, upstream502Handler())
	firstRoute, ok := server.lookupChannel("breaker-channel")
	if !ok {
		t.Fatal("missing first route")
	}
	server.SetRoutes([]config.Route{
		firstRoute,
		facadeRoute("other-channel", "responses", "wire", "key2", second.URL),
	})

	for i := 0; i < circuitOpenThreshold; i++ {
		postFacade(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("first channel must fast-fail, status=%d", status)
	}
	if _, status, _ := postFacadeResponse(t, server, "other-channel", []byte(breakerProbeBody), ""); status != http.StatusBadGateway {
		t.Fatalf("second channel must stay unaffected, status=%d", status)
	}
	if got := atomic.LoadInt64(&secondCalls); got != 1 {
		t.Fatalf("second channel upstream calls=%d, want 1", got)
	}
}

func TestBreakerIgnoresNonRetryableFailures(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_model","message":"invalid model"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)

	for i := 0; i < circuitOpenThreshold+3; i++ {
		_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		if status != http.StatusBadRequest {
			t.Fatalf("attempt %d: status=%d, want 400", i+1, status)
		}
		if header.Get("X-Should-Retry") != "false" {
			t.Fatalf("attempt %d: structured invalid model must be non-retryable", i+1)
		}
	}
	if got := atomic.LoadInt64(calls); got != circuitOpenThreshold+3 {
		t.Fatalf("non-retryable failures must keep forwarding: calls=%d", got)
	}
}

func TestBreakerRateLimitResetsStreak(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"rate limited"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)

	for i := 0; i < 12; i++ {
		if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=%d, want 429", i+1, status)
		}
	}
	if got := atomic.LoadInt64(calls); got != 12 {
		t.Fatalf("429s prove the channel is responsive and must not open the breaker: calls=%d", got)
	}
}

func TestBreakerTransportErrorsCount(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("expected hijacker")
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	server, calls := newBreakerTestServer(t, handler)

	for i := 0; i < circuitOpenThreshold; i++ {
		_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		if status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status=%d, want 502", i+1, status)
		}
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("transport errors must open the breaker, status=%d", status)
	}
	if got := atomic.LoadInt64(calls); got != circuitOpenThreshold {
		t.Fatalf("open breaker must not hit upstream: calls=%d", got)
	}
}

func TestBreakerResetOnEnable(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstream502Handler())
	for i := 0; i < circuitOpenThreshold; i++ {
		postFacade(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("breaker must be open, status=%d", status)
	}

	server.Enable()
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusBadGateway {
		t.Fatalf("Enable must reset breakers, status=%d", status)
	}
	if got := atomic.LoadInt64(calls); got != circuitOpenThreshold+1 {
		t.Fatalf("upstream calls=%d, want %d", got, circuitOpenThreshold+1)
	}
}

func containsJSONType(data []byte, wanted string) bool {
	return strings.Contains(string(data), `"`+wanted+`"`)
}
