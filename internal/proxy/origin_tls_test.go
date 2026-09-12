package proxy

import (
	"io"
	"net/http"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

const cfOriginTLSHTML = `<html><head><title>relay.example | 525: SSL Handshake Failed</title></head><body>525</body></html>`

func originTLSHandler(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, cfOriginTLSHTML)
	}
}

// A relay origin-TLS failure (Cloudflare 525) routinely clears within the
// absorb window: origin restarts and certificate rotations are transient,
// and the failed handshake never reached the origin application. The absorb
// layer must hide it, even though Grok Build itself treats 525 as terminal
// and would fail the turn on sight.
func TestOriginTLS525AbsorbedUntilRecovery(t *testing.T) {
	var served int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		if served <= 2 {
			originTLSHandler(statusOriginTLSHandshake)(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, nativeSuccessBody("responses", "wire", "OK"))
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	data, status, _ := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want the absorbed 525s to surface the recovered 200)", status, data)
	}
	if got := *calls; got != 3 {
		t.Fatalf("upstream calls=%d want 3 (two 525 attempts plus the recovery)", got)
	}
}

// When the origin-TLS failure outlasts the absorb window it passes through
// with Grok Build's terminal disposition: no client retry is suggested for a
// status whose client-side policy is terminal, and the raw edge page stays
// visible in diagnostics.
func TestOriginTLS525PassthroughIsTerminal(t *testing.T) {
	server, calls := newBreakerTestServer(t, originTLSHandler(statusOriginTLSHandshake))
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 5
	})
	stubAbsorbClock(server)

	data, status, header := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != statusOriginTLSHandshake {
		t.Fatalf("status=%d body=%s (want the 525 passed through after the window)", status, data)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("origin-TLS passthrough must stay terminal for Grok Build, got %q", header.Get("X-Should-Retry"))
	}
	// Budget 5s: waits of 2s and 3s (capped) fit three attempts, then the
	// window is exhausted.
	if got := *calls; got != 3 {
		t.Fatalf("upstream calls=%d want 3 inside the 5s window", got)
	}
}

// Transient Cloudflare edge pages (520-524, 529, 530) are retryable in Grok
// Build's edge-client policy. hellogrok must not stamp X-Should-Retry: false
// on them: the header vetoes Grok Build's own retry and turns a self-clearing
// edge blip into an instant turn failure.
func TestCloudflareEdge522StaysRetryableForClient(t *testing.T) {
	server, calls := newBreakerTestServer(t, originTLSHandler(522))
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 0 // observe the passthrough disposition directly
	})

	data, status, header := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != 522 {
		t.Fatalf("status=%d body=%s (want the 522 passed through)", status, data)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("edge 522 must stay retryable for Grok Build, got %q", header.Get("X-Should-Retry"))
	}
	if header.Get("Retry-After") != "30" {
		t.Fatalf("edge 522 must carry the synthesized Retry-After, got %q", header.Get("Retry-After"))
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1 with the absorb layer disabled", got)
	}
}
