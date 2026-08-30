package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func testAbsorbRoute(maxSecs uint64, configured bool, backoffCapSecs uint64) config.Route {
	return config.Route{
		AbsorbRetryMaxSecs:        maxSecs,
		AbsorbRetryMaxConfigured:  configured,
		AbsorbRetryBackoffCapSecs: backoffCapSecs,
	}
}

func TestAbsorbBudgetDefaultsAndDisable(t *testing.T) {
	if got := absorbBudget(testAbsorbRoute(0, false, 0)); got != defaultAbsorbRetryMaxSecs*time.Second {
		t.Fatalf("default budget=%v, want %v", got, defaultAbsorbRetryMaxSecs*time.Second)
	}
	if got := absorbBudget(testAbsorbRoute(0, true, 0)); got != 0 {
		t.Fatalf("explicit zero must disable the absorb layer, got %v", got)
	}
	if got := absorbBudget(testAbsorbRoute(12, true, 0)); got != 12*time.Second {
		t.Fatalf("explicit budget=%v, want 12s", got)
	}
}

func TestAbsorbBackoffProgressionAndCap(t *testing.T) {
	state := newAbsorbState(testAbsorbRoute(300, true, 30))
	if got := state.nextWait(0); got != 2*time.Second {
		t.Fatalf("first backoff=%v, want 2s", got)
	}
	state.attempt = 1
	if got := state.nextWait(0); got != 4*time.Second {
		t.Fatalf("second backoff=%v, want 4s", got)
	}
	state.attempt = 10
	if got := state.nextWait(0); got != 30*time.Second {
		t.Fatalf("capped backoff=%v, want 30s", got)
	}
}

func TestAbsorbBackoffCapFromRoute(t *testing.T) {
	state := newAbsorbState(testAbsorbRoute(300, true, 4))
	state.attempt = 3
	if got := state.nextWait(0); got != 4*time.Second {
		t.Fatalf("route-capped backoff=%v, want 4s", got)
	}
}

func TestAbsorbWaitCapsAtRemainingBudget(t *testing.T) {
	clock := time.Now()
	state := newAbsorbState(testAbsorbRoute(3, true, 30))
	state.now = func() time.Time { return clock }
	state.sleep = func(ctx context.Context, d time.Duration) bool {
		clock = clock.Add(d)
		return true
	}
	if !state.wait(context.Background(), 0) {
		t.Fatal("first wait must fit the budget")
	}
	if state.lastWait != 2*time.Second {
		t.Fatalf("first wait=%v, want 2s", state.lastWait)
	}
	if !state.wait(context.Background(), 0) {
		t.Fatal("second wait must fit the remaining budget")
	}
	if state.lastWait != 1*time.Second {
		t.Fatalf("second wait=%v, want 1s (remaining budget)", state.lastWait)
	}
	if state.wait(context.Background(), 0) {
		t.Fatal("third wait must report budget exhaustion")
	}
}

func TestAbsorbWaitHonorsRetryAfter(t *testing.T) {
	clock := time.Now()
	state := newAbsorbState(testAbsorbRoute(10, true, 30))
	state.now = func() time.Time { return clock }
	state.sleep = func(ctx context.Context, d time.Duration) bool {
		clock = clock.Add(d)
		return true
	}
	if !state.wait(context.Background(), 7) {
		t.Fatal("retry-after wait must fit the budget")
	}
	if state.lastWait != 7*time.Second {
		t.Fatalf("wait=%v, want 7s from Retry-After", state.lastWait)
	}
	if !state.wait(context.Background(), 120) {
		t.Fatal("second wait must fit the budget")
	}
	if state.lastWait != 3*time.Second {
		t.Fatalf("wait=%v, want 3s (remaining budget beats oversized Retry-After)", state.lastWait)
	}
}

func TestAbsorbWaitStopsOnCancelledContext(t *testing.T) {
	state := newAbsorbState(testAbsorbRoute(90, true, 30))
	state.now = func() time.Time { return time.Now() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if state.wait(ctx, 0) {
		t.Fatal("cancelled context must stop the absorb wait")
	}
}

func TestAbsorbRetryRecoversWithRealSleep(t *testing.T) {
	failures := atomic.Int64{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failures.Add(1) == 1 {
			upstreamBusyHandler().ServeHTTP(w, r)
			return
		}
		upstreamOKHandler().ServeHTTP(w, r)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxSecs = 3
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryBackoffCapSecs = 2
	})

	started := time.Now()
	_, status, _ := postFacadeResponse(t, server, "breaker-channel", []byte(breakerProbeBody), "")
	if status != http.StatusOK {
		t.Fatalf("absorbed busy failure must surface success, status=%d", status)
	}
	if elapsed := time.Since(started); elapsed < 1500*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("absorb wait took %s, want roughly the 2s backoff step", elapsed)
	}
	if got := atomic.LoadInt64(calls); got != 2 {
		t.Fatalf("upstream calls=%d, want 2", got)
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	header := http.Header{}
	if got := retryAfterSeconds(header); got != 0 {
		t.Fatalf("absent Retry-After=%d, want 0", got)
	}
	header.Set("Retry-After", "25")
	if got := retryAfterSeconds(header); got != 25 {
		t.Fatalf("Retry-After=%d, want 25", got)
	}
	header.Set("Retry-After", "Sun, 06 Nov 1994 08:49:37 GMT")
	if got := retryAfterSeconds(header); got != 0 {
		t.Fatalf("HTTP-date Retry-After=%d, want 0", got)
	}
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
