package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"syscall"
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

func upstreamBusyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"The service is busy. Wait a minute and send again."}}`)
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

// setRoute replaces the test channel's route with a modified copy.
func setBreakerTestRoute(t *testing.T, server *Server, modify func(*config.Route)) {
	t.Helper()
	route, ok := server.lookupChannel("breaker-channel")
	if !ok {
		t.Fatal("missing breaker-channel route")
	}
	modify(&route)
	server.SetRoutes([]config.Route{route})
}

// stubAbsorbClock makes the absorb layer and dead-channel breaker use a
// virtual clock: sleeps advance the clock instantly instead of waiting.
func stubAbsorbClock(server *Server) *time.Time {
	clock := time.Now()
	server.absorbNow = func() time.Time { return clock }
	server.absorbSleep = func(ctx context.Context, d time.Duration) bool {
		clock = clock.Add(d)
		return ctx.Err() == nil
	}
	server.breakers.now = func() time.Time { return clock }
	return &clock
}

// A probe released by allow() that never reports a result (client
// disconnected mid-probe, or the response headers timed out: neither calls
// recordSuccess/recordFailure) must not latch probing forever. The lease lets
// a fresh probe through after the previous one is presumed lost.
func TestBreakerLostProbeLeaseReleasesProbing(t *testing.T) {
	clock := time.Now()
	store := newBreakerStore()
	store.now = func() time.Time { return clock }
	params := breakerParams{Enabled: true, Threshold: 2}
	const channel = "c"

	store.recordFailure(channel, params)
	if got := store.recordFailure(channel, params); got != breakerOpened {
		t.Fatalf("second failure transition=%v, want breakerOpened", got)
	}
	// In cooldown: rejected.
	if store.allow(channel, params) {
		t.Fatal("allow inside cooldown should be false")
	}
	// After cooldown: a probe is released.
	clock = clock.Add(deadChannelCooldown + time.Second)
	if !store.allow(channel, params) {
		t.Fatal("first probe after cooldown should be allowed")
	}
	// A concurrent request while the probe is in flight is rejected.
	if store.allow(channel, params) {
		t.Fatal("allow while probe in flight should be false")
	}
	// The probe is lost (no record call). Before the lease expires, still rejected.
	clock = clock.Add(deadChannelProbeLease - time.Second)
	if store.allow(channel, params) {
		t.Fatal("allow before probe lease expiry should be false")
	}
	// After the lease, a fresh probe is released instead of latching forever.
	clock = clock.Add(2 * time.Second)
	if !store.allow(channel, params) {
		t.Fatal("allow after probe lease expiry should release a fresh probe, not stay latched")
	}
}

// deadUpstreamURL returns a URL that refuses connections.
func deadUpstreamURL(t *testing.T) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := upstream.URL
	upstream.Close()
	return url
}

// pointRouteAtDeadUpstream rewires the test route to a URL that refuses
// connections.
func pointRouteAtDeadUpstream(t *testing.T, route *config.Route) {
	t.Helper()
	parsed, err := url.Parse(deadUpstreamURL(t))
	if err != nil {
		t.Fatal(err)
	}
	route.OriginBase = parsed.String()
	route.Host = parsed.Host
}

func TestAbsorbRetryRecoversBeforeClientSeesFailure(t *testing.T) {
	failures := atomic.Int64{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) <= 2 {
			upstreamBusyHandler().ServeHTTP(w, r)
			return
		}
		upstreamOKHandler().ServeHTTP(w, r)
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusOK {
		t.Fatalf("absorbed busy failures must surface success, status=%d", status)
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("upstream calls=%d, want 3 (two absorbed retries)", got)
	}
}

func TestAbsorbBudgetExhaustedPassesThroughRetryable(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 5
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("budget-exhausted status=%d, want 503", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("budget-exhausted failure must stay retryable for Grok Build, got %q", header.Get("X-Should-Retry"))
	}
	if got := header.Get("Retry-After"); got != "30" {
		t.Fatalf("passthrough must pace Grok Build with a synthesized Retry-After, got %q", got)
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("upstream calls=%d, want 3 (2s+3s of absorbed waits inside the 5s budget)", got)
	}
}

func TestAbsorbDisabledByExplicitZero(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 0
		route.AbsorbRetryMaxConfigured = true
	})

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("disabled absorb must pass the failure through, status=%d", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("passthrough must stay retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("upstream calls=%d, want 1 (no proxy-side retry)", got)
	}
}

func TestAbsorbHonorsUpstreamRetryAfter(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"busy"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 5
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 after budget", status)
	}
	// Retry-After 2s paces the absorbed waits (2+2+1s inside the 5s budget);
	// exponential backoff would only fit three attempts.
	if got := atomic.LoadInt64(calls); got != 4 {
		t.Fatalf("upstream calls=%d, want 4 (Retry-After pacing)", got)
	}
}

func TestTransportErrorPassesThroughWithoutAbsorb(t *testing.T) {
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

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusBadGateway {
		t.Fatalf("transport failure status=%d, want 502", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("transport failure must stay retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("transport errors must not be absorbed: calls=%d, want 1", got)
	}
}

// The off default passes deterministic errors straight through instead of
// entering the absorb window; only the opt-in balanced tier absorbs them.
func TestDeterministicErrorSkipsAbsorbInOffTier(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_model","message":"invalid model"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {})

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", status)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("deterministic failures must be non-retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("deterministic failures must not be absorbed in the off tier: calls=%d, want 1", got)
	}
}

func TestDeadChannelFailFastOptIn(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstream502Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		pointRouteAtDeadUpstream(t, route)
		route.DeadChannelFailFast = true
	})
	stubAbsorbClock(server)

	for i := 0; i < defaultDeadChannelFailThreshold; i++ {
		_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		if status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status=%d, want 502", i+1, status)
		}
		if header.Get("X-Should-Retry") != "true" {
			t.Fatalf("attempt %d: dial failure must stay retryable, got %q", i+1, header.Get("X-Should-Retry"))
		}
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
}

func TestDeadChannelBreakerIgnoresSoftFailures(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.DeadChannelFailFast = true
		route.AbsorbRetryMaxSecs = 0
		route.AbsorbRetryMaxConfigured = true
	})

	for i := 0; i < 12; i++ {
		data, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
		if status != http.StatusServiceUnavailable {
			t.Fatalf("attempt %d: status=%d, want 503", i+1, status)
		}
		if header.Get("X-Should-Retry") != "true" {
			t.Fatalf("attempt %d: soft failure must stay retryable, got %q", i+1, header.Get("X-Should-Retry"))
		}
		if containsJSONType(data, "proxy_circuit_open") {
			t.Fatalf("attempt %d: soft failures must never open the dead-channel breaker", i+1)
		}
	}
	if got := atomic.LoadInt64(calls); got != 12 {
		t.Fatalf("upstream calls=%d, want 12", got)
	}
}

func TestDeadChannelThresholdFromConfig(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstream502Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		pointRouteAtDeadUpstream(t, route)
		route.DeadChannelFailFast = true
		route.DeadChannelFailThreshold = 2
	})
	stubAbsorbClock(server)

	for i := 0; i < 2; i++ {
		if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusBadGateway {
			t.Fatalf("attempt %d: status=%d, want 502", i+1, status)
		}
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("configured threshold must fast-fail, status=%d", status)
	}
}

func TestDeadChannelBreakerProbeRecoversAfterCooldown(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstream502Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		pointRouteAtDeadUpstream(t, route)
		route.DeadChannelFailFast = true
		route.DeadChannelFailThreshold = 2
	})
	clock := stubAbsorbClock(server)

	for i := 0; i < 2; i++ {
		postFacade(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("in-cooldown status=%d, want 503", status)
	}

	// After the cooldown the first request becomes a probe; it still fails,
	// so the cooldown re-arms and the next request fast-fails again.
	*clock = clock.Add(deadChannelCooldown + time.Second)
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusBadGateway {
		t.Fatalf("failed probe status=%d, want 502", status)
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("re-armed fast-fail status=%d, want 503", status)
	}

	// The channel recovers; the next probe succeeds and closes the breaker.
	*clock = clock.Add(deadChannelCooldown + time.Second)
	live := httptest.NewServer(upstreamOKHandler())
	defer live.Close()
	parsedLive, err := url.Parse(live.URL)
	if err != nil {
		t.Fatal(err)
	}
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.OriginBase = parsedLive.String()
		route.Host = parsedLive.Host
	})
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusOK {
		t.Fatalf("recovering probe status=%d, want 200", status)
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusOK {
		t.Fatalf("post-recovery status=%d, want 200", status)
	}
}

func TestDeadChannelBreakerResetsOnEnable(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstream502Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		pointRouteAtDeadUpstream(t, route)
		route.DeadChannelFailFast = true
		route.DeadChannelFailThreshold = 2
	})
	stubAbsorbClock(server)

	for i := 0; i < 2; i++ {
		postFacade(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	}
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusServiceUnavailable {
		t.Fatalf("breaker must be open, status=%d", status)
	}

	server.Enable()
	if _, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), ""); status != http.StatusBadGateway {
		t.Fatalf("Enable must reset breakers, status=%d", status)
	}
}

func containsJSONType(data []byte, wanted string) bool {
	return strings.Contains(string(data), `"`+wanted+`"`)
}

func TestIsHardUpstreamError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"dns failure", &net.DNSError{Name: "dead.example", Err: "no such host"}, true},
		{"tls certificate", &tls.CertificateVerificationError{Err: fmt.Errorf("bad cert")}, true},
		{"tls unknown authority", x509.UnknownAuthorityError{Cert: nil}, true},
		{"tls record header", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, true},
		{"tls remote alert", &net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, true},
		{"tls local error", &net.OpError{Op: "local error", Err: errors.New("tls: no cipher suite in common")}, true},
		{"tls remote alert marked timeout", &net.OpError{Op: "remote error", Err: context.DeadlineExceeded}, false},
		{"https to http server", errors.New("http: server gave HTTP response to HTTPS client"), true},
		{"wrapped https to http server", fmt.Errorf("round trip: %w", errors.New("http: server gave HTTP response to HTTPS client")), true},
		{"dial timeout", &net.OpError{Op: "dial", Err: context.DeadlineExceeded}, false},
		{"read reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, false},
		{"generic transport", fmt.Errorf("server closed idle connection"), false},
	}
	for _, test := range cases {
		if got := isHardUpstreamError(test.err); got != test.want {
			t.Errorf("%s: isHardUpstreamError=%v, want %v", test.name, got, test.want)
		}
	}
}
