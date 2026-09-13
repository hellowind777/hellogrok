package proxy

import (
	"context"
	"net/http"
	"sync/atomic"
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

func TestAbsorbBudgetDefaultsTo90Seconds(t *testing.T) {
	if defaultAbsorbRetryMaxSecs != 90 {
		t.Fatalf("defaultAbsorbRetryMaxSecs=%d, want 90", defaultAbsorbRetryMaxSecs)
	}
	if got := absorbBudget(testAbsorbRoute(0, false, 0)); got != 90*time.Second {
		t.Fatalf("default budget=%v, want 90s", got)
	}
}

// The off tier no longer disables the window: soft-failure absorption is
// always on, and only absorb_retry_max_secs = 0 opts a channel out.
func TestAbsorbBudgetIgnoresResilienceTier(t *testing.T) {
	route := testAbsorbRoute(0, false, 0)
	route.ErrorResilience = "off"
	if got := absorbBudget(route); got != defaultAbsorbRetryMaxSecs*time.Second {
		t.Fatalf("off tier budget=%v, want the default soft-failure window", got)
	}
	route = testAbsorbRoute(120, true, 30)
	route.ErrorResilience = "balanced"
	if got := absorbBudget(route); got != 120*time.Second {
		t.Fatalf("explicit window budget=%v, want 120s", got)
	}
}

func TestRouteResilienceTierMapping(t *testing.T) {
	cases := []struct {
		value string
		want  errorResilience
	}{
		{"", resilienceOff},
		{"off", resilienceOff},
		{"OFF", resilienceOff},
		{"balanced", resilienceBalanced},
		{"BALANCED", resilienceBalanced},
	}
	for _, test := range cases {
		route := config.Route{ErrorResilience: test.value}
		if got := routeResilience(route); got != test.want {
			t.Errorf("error_resilience %q tier=%v, want %v", test.value, got, test.want)
		}
	}
}

func TestAbsorbEligibilityByTier(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryable  bool
		resilience errorResilience
		want       bool
	}{
		{"408 retryable off", http.StatusRequestTimeout, true, resilienceOff, true},
		{"408 non-retryable off", http.StatusRequestTimeout, false, resilienceOff, true},
		{"408 balanced", http.StatusRequestTimeout, true, resilienceBalanced, true},
		{"525 off", statusOriginTLSHandshake, false, resilienceOff, true},
		{"401 balanced", http.StatusUnauthorized, false, resilienceBalanced, true},
		{"400 balanced", http.StatusBadRequest, false, resilienceBalanced, true},
		{"422 balanced", http.StatusUnprocessableEntity, false, resilienceBalanced, true},
		{"401 off", http.StatusUnauthorized, false, resilienceOff, false},
		{"400 off", http.StatusBadRequest, false, resilienceOff, false},
		{"404 off", http.StatusNotFound, false, resilienceOff, false},
		{"503 retryable off", http.StatusServiceUnavailable, true, resilienceOff, true},
	}
	for _, test := range cases {
		if got := absorbEligible(test.status, test.retryable, test.resilience); got != test.want {
			t.Errorf("%s: absorbEligible=%v, want %v", test.name, got, test.want)
		}
	}
}

func TestAbsorbFamilyClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   absorbFamily
	}{
		{"response-header timeout", 0, familyTimeout},
		{"408", http.StatusRequestTimeout, familyTimeout},
		{"504", http.StatusGatewayTimeout, familyTimeout},
		{"525", statusOriginTLSHandshake, familyTimeout},
		{"429", http.StatusTooManyRequests, familyRateLimit},
		{"503", http.StatusServiceUnavailable, familyRateLimit},
		{"401", http.StatusUnauthorized, familyAuth},
		{"403", http.StatusForbidden, familyAuth},
		{"400", http.StatusBadRequest, familyValidation},
		{"404", http.StatusNotFound, familyValidation},
		{"422", http.StatusUnprocessableEntity, familyValidation},
	}
	for _, test := range cases {
		if got := absorbFamilyFor(test.status, false, ""); got != test.want {
			t.Errorf("%s: family=%v, want %v", test.name, got, test.want)
		}
	}
}

func TestAbsorbAuthAndValidationUseFixedSemanticWaits(t *testing.T) {
	for _, family := range []absorbFamily{familyAuth, familyValidation} {
		state := newAbsorbState(testAbsorbRoute(300, true, 4))
		if got := state.nextWait(family, 0); got != 30*time.Second {
			t.Fatalf("family %v first wait=%v, want 30s (backoff cap must not apply)", family, got)
		}
		state.attempt = 1
		if got := state.nextWait(family, 0); got != 60*time.Second {
			t.Fatalf("family %v second wait=%v, want 60s", family, got)
		}
		state.attempt = 5
		if got := state.nextWait(family, 0); got != 60*time.Second {
			t.Fatalf("family %v later wait=%v, want fixed 60s", family, got)
		}
		// An upstream Retry-After still wins over the fixed sequence.
		if got := state.nextWait(family, 25); got != 25*time.Second {
			t.Fatalf("family %v retry-after wait=%v, want 25s", family, got)
		}
	}
}

func TestAbsorbBackoffProgressionAndCap(t *testing.T) {
	state := newAbsorbState(testAbsorbRoute(300, true, 30))
	if got := state.nextWait(familyTimeout, 0); got != 2*time.Second {
		t.Fatalf("first backoff=%v, want 2s", got)
	}
	state.attempt = 1
	if got := state.nextWait(familyTimeout, 0); got != 4*time.Second {
		t.Fatalf("second backoff=%v, want 4s", got)
	}
	state.attempt = 10
	if got := state.nextWait(familyTimeout, 0); got != 30*time.Second {
		t.Fatalf("capped backoff=%v, want 30s", got)
	}
}

func TestAbsorbBackoffCapFromRoute(t *testing.T) {
	state := newAbsorbState(testAbsorbRoute(300, true, 4))
	state.attempt = 3
	if got := state.nextWait(familyTimeout, 0); got != 4*time.Second {
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
	if !state.wait(context.Background(), familyTimeout, 0) {
		t.Fatal("first wait must fit the budget")
	}
	if state.lastWait != 2*time.Second {
		t.Fatalf("first wait=%v, want 2s", state.lastWait)
	}
	if !state.wait(context.Background(), familyTimeout, 0) {
		t.Fatal("second wait must fit the remaining budget")
	}
	if state.lastWait != 1*time.Second {
		t.Fatalf("second wait=%v, want 1s (remaining budget)", state.lastWait)
	}
	if state.wait(context.Background(), familyTimeout, 0) {
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
	if !state.wait(context.Background(), familyRateLimit, 7) {
		t.Fatal("retry-after wait must fit the budget")
	}
	if state.lastWait != 7*time.Second {
		t.Fatalf("wait=%v, want 7s from Retry-After", state.lastWait)
	}
	if !state.wait(context.Background(), familyRateLimit, 120) {
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
	if state.wait(ctx, familyTimeout, 0) {
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
