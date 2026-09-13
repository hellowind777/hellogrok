package proxy

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// These tests exercise the per-channel error_resilience tiers and the 408
// remap through the full facade, using the breaker_test harness.

func upstream408Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"edge gateway timeout"}}`)
	}
}

func upstream401Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"channel key rotated"}}`)
	}
}

func TestUpstream408RemapsToRetryable504(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstream408Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 3
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("408 passthrough status=%d, want 504", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("408 passthrough must be retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("upstream calls=%d, want 3 (2s+1s of absorbed retries inside the 3s window)", got)
	}
}

func TestBalancedTierAbsorbsDeterministicAuthFailure(t *testing.T) {
	failures := atomic.Int64{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) == 1 {
			upstream401Handler().ServeHTTP(w, r)
			return
		}
		upstreamOKHandler().ServeHTTP(w, r)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.ErrorResilience = "balanced"
		route.AbsorbRetryMaxSecs = 300
		route.AbsorbRetryMaxConfigured = true
	})
	clock := stubAbsorbClock(server)
	started := *clock

	_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusOK {
		t.Fatalf("absorbed 401 must surface success, status=%d", status)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Fatalf("upstream calls=%d, want 2 (one absorbed auth retry)", got)
	}
	if elapsed := clock.Sub(started); elapsed != 30*time.Second {
		t.Fatalf("auth absorb wait=%v, want the fixed 30s first wait", elapsed)
	}
}

// An unconfigured channel (the off default) passes deterministic failures
// straight through with the provider's explanation.
func TestOffTierSkipsDeterministicAuthFailure(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstream401Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 300
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusUnauthorized {
		t.Fatalf("off tier status=%d, want 401 passthrough", status)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("off tier deterministic failure must stay non-retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 1 {
		t.Fatalf("off tier upstream calls=%d, want 1 (no proxy-side retry)", got)
	}
}

// Soft-failure absorption is always on, including for unconfigured
// channels: a busy 503 is retried inside the proxy until the window is
// exhausted and only then passes through retryable.
func TestOffTierAbsorbsSoftFailureInsideWindow(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 3
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("off tier status=%d, want 503 passthrough after the window", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("off tier passthrough must stay retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("off tier upstream calls=%d, want 3 (2s+1s of absorbed retries inside the 3s window)", got)
	}
}

// The off tier still rewrites a 408 to a retryable 504 on the way out after
// its absorb window is exhausted.
func TestOffTierRemaps408AfterWindow(t *testing.T) {
	server, calls := newBreakerTestServer(t, upstream408Handler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 3
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	_, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusGatewayTimeout {
		t.Fatalf("off tier 408 passthrough status=%d, want 504", status)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("off tier 408 passthrough must be retryable, got %q", header.Get("X-Should-Retry"))
	}
	if got := atomic.LoadInt64(calls); got != 3 {
		t.Fatalf("off tier upstream calls=%d, want 3 (absorbed retries inside the 3s window)", got)
	}
}

// A failure that outlasted a long absorb window tells the user how stale it
// is: a delay header always, and a one-line prefix on structured error
// messages.
func TestMarkAbsorbedFailureAnnotatesStaleError(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 45
		route.AbsorbRetryMaxConfigured = true
	})
	clock := stubAbsorbClock(server)
	started := *clock

	body, status, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", status)
	}
	elapsed := clock.Sub(started)
	if elapsed < 30*time.Second {
		t.Fatalf("virtual elapsed=%v, want the absorb window to have been consumed", elapsed)
	}
	if header.Get("X-Hellogrok-Absorb-Delay") != "45" {
		t.Fatalf("X-Hellogrok-Absorb-Delay=%q, want 45 (headers=%v)", header.Get("X-Hellogrok-Absorb-Delay"), header)
	}
	root, err := decodeJSONMap(body)
	if err != nil {
		t.Fatalf("body is no longer a JSON envelope: %v", err)
	}
	message := firstString(root["error"].(map[string]any), "message")
	if !strings.HasPrefix(message, "[hellogrok: upstream stayed failing for ") || !strings.Contains(message, "The service is busy") {
		t.Fatalf("message=%q", message)
	}
}

// Short absorb windows stay invisible: the client receives the upstream
// error body byte-for-byte with no annotation.
func TestMarkAbsorbedFailureSkipsShortDelays(t *testing.T) {
	server, _ := newBreakerTestServer(t, upstreamBusyHandler())
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 3
		route.AbsorbRetryMaxConfigured = true
	})
	stubAbsorbClock(server)

	body, _, header := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if header.Get("X-Hellogrok-Absorb-Delay") != "" {
		t.Fatalf("short window must not be annotated, headers=%v", header)
	}
	if strings.Contains(string(body), "[hellogrok:") {
		t.Fatalf("short window body annotated: %s", body)
	}
}
