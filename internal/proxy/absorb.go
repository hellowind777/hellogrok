package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// The absorb layer hides transient upstream soft failures from Grok Build by
// waiting and retrying inside the proxy while the upstream response headers
// have not been sent. Replaying the request at that point is side-effect
// free: the upstream has either rejected the request outright or never
// completed it. The client's own retry budget (15 attempts, ~5.5 minutes) is
// never consumed while the window lasts; when the window is exhausted the
// original failure passes through as retryable and the client keeps its full
// budget. Soft failures include busy/overloaded 5xx statuses, 429s, and
// response-header timeouts. Transport errors are not absorbed: Grok Build's
// first-retry client rebuild (HTTP/1.1 fallback) handles those better than a
// proxy-internal replay could.
//
// The per-channel error_resilience tier selects how far the layer goes.
// Soft-failure absorption (retryable 5xx, 429, 408 edge-timeout pages,
// origin-TLS 525/526, response-header timeouts) is always on: those failures
// clear on their own and Grok Build's retry of them only burns time. An
// unconfigured channel ("off") additionally passes deterministic 4xx
// failures (auth, validation) straight through with the provider's
// explanation, because those never succeed on retry; "balanced" absorbs them
// too with fixed 30s-then-60s waits, so an unattended turn survives
// relay-side token rotation, permission fixes, or config reloads.
const (
	// The default window absorbs momentary soft failures before the client
	// sees anything; once exhausted, the failure passes through retryable
	// and Grok Build's own retry loop takes over, showing the error reason
	// alongside its retry indicator. Per-channel absorb_retry_max_secs
	// still overrides it and an explicit 0 disables the layer; an upstream
	// Retry-After stays separately capped at absorbMaxUpstreamRetryAfter.
	defaultAbsorbRetryMaxSecs        = 90
	defaultAbsorbRetryBackoffCapSecs = 30
	absorbMinBackoff                 = 2 * time.Second
	absorbMaxUpstreamRetryAfter      = 60 * time.Second
	// synthesizedRetryAfterSeconds paces Grok Build's own retries when an
	// upstream busy/overloaded error carried no Retry-After of its own. Grok
	// Build caps a Retry-After hint at 30 seconds, so larger values gain
	// nothing.
	synthesizedRetryAfterSeconds = 30
	// absorbSemanticFirstWait and absorbSemanticRepeatWait are the fixed
	// wait sequence for auth and validation failures: the first retry waits
	// 30 seconds and every later one 60, so token rotation, permission
	// fixes, or provider config changes have time to land. The backoff cap
	// does not apply because the sequence is fixed rather than exponential.
	absorbSemanticFirstWait  = 30 * time.Second
	absorbSemanticRepeatWait = 60 * time.Second
)

// errorResilience is the parsed per-channel error_resilience tier. The off
// tier (soft failures only) is the default for an unconfigured channel.
type errorResilience int

const (
	resilienceOff errorResilience = iota
	resilienceBalanced
)

// routeResilience maps the route's error_resilience value to its tier. The
// config layer already normalizes it; the runtime still trims and lowercases
// so hand-built routes get the same treatment.
func routeResilience(route config.Route) errorResilience {
	if strings.EqualFold(strings.TrimSpace(route.ErrorResilience), "balanced") {
		return resilienceBalanced
	}
	return resilienceOff
}

// absorbFamily groups a failure into its wait family. timeout and rate-limit
// failures keep the exponential sequence; auth and validation failures use
// the fixed semantic sequence.
type absorbFamily int

const (
	familyTimeout    absorbFamily = iota // response-header timeout, 408, 504, 52x
	familyRateLimit                      // 429, 503
	familyAuth                           // 401, 403
	familyValidation                     // remaining 4xx
)

// absorbFamilyFor returns the family for a concrete failure. status alone
// decides, including the wrapped deterministic envelope's surrogate status;
// retryable and reason are accepted so call sites can thread their
// disposition and log label through one expression.
func absorbFamilyFor(status int, retryable bool, reason string) absorbFamily {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return familyAuth
	}
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return familyRateLimit
	}
	if status >= http.StatusInternalServerError || status == http.StatusRequestTimeout {
		return familyTimeout
	}
	if status >= http.StatusBadRequest {
		return familyValidation
	}
	// No status: the response-header timeout call site.
	return familyTimeout
}

func absorbBudget(route config.Route) time.Duration {
	if route.AbsorbRetryMaxConfigured {
		// An explicit 0 disables the absorb layer for the channel.
		return time.Duration(route.AbsorbRetryMaxSecs) * time.Second
	}
	return defaultAbsorbRetryMaxSecs * time.Second
}

func absorbBackoffCap(route config.Route) time.Duration {
	if route.AbsorbRetryBackoffCapSecs > 0 {
		return time.Duration(route.AbsorbRetryBackoffCapSecs) * time.Second
	}
	return defaultAbsorbRetryBackoffCapSecs * time.Second
}

// retryAfterSeconds reads a delta-seconds Retry-After header. HTTP-date
// values are ignored, matching Grok Build's own parsing.
func retryAfterSeconds(header http.Header) int {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0
	}
	return seconds
}

// absorbState tracks one request's wait-and-retry window. Budget, backoff,
// and tier come from the route configuration; now and sleep are injectable
// for tests.
type absorbState struct {
	budget     time.Duration // 0 = disabled
	backoffCap time.Duration
	resilience errorResilience
	now        func() time.Time
	sleep      func(context.Context, time.Duration) bool
	started    time.Time
	attempt    int
	lastWait   time.Duration
}

func newAbsorbState(route config.Route) *absorbState {
	return &absorbState{
		budget:     absorbBudget(route),
		backoffCap: absorbBackoffCap(route),
		resilience: routeResilience(route),
	}
}

// elapsed reports how much of the budget this request has already waited.
func (a *absorbState) elapsed() time.Duration {
	if a.now == nil || a.started.IsZero() {
		return 0
	}
	return a.now().Sub(a.started)
}

// wait pauses for the next wait interval or the remaining budget, whichever
// is smaller. family selects the wait sequence; retryAfterSeconds comes from
// the upstream error response when present (capped) and always wins over the
// family's sequence. It returns false when the budget is exhausted or the
// context is done, meaning the caller must pass the failure through to the
// client.
func (a *absorbState) wait(ctx context.Context, family absorbFamily, retryAfter int) bool {
	if a.now == nil {
		a.now = time.Now
	}
	if a.sleep == nil {
		a.sleep = sleepWithContext
	}
	if a.started.IsZero() {
		a.started = a.now()
	}
	elapsed := a.now().Sub(a.started)
	if a.budget <= 0 || elapsed >= a.budget {
		return false
	}
	wait := a.nextWait(family, retryAfter)
	if remaining := a.budget - elapsed; wait > remaining {
		wait = remaining
	}
	a.attempt++
	a.lastWait = wait
	return a.sleep(ctx, wait)
}

func (a *absorbState) nextWait(family absorbFamily, retryAfter int) time.Duration {
	if retryAfter > 0 {
		wait := time.Duration(retryAfter) * time.Second
		if wait > absorbMaxUpstreamRetryAfter {
			return absorbMaxUpstreamRetryAfter
		}
		return wait
	}
	if family == familyAuth || family == familyValidation {
		if a.attempt == 0 {
			return absorbSemanticFirstWait
		}
		return absorbSemanticRepeatWait
	}
	wait := absorbMinBackoff
	for i := 0; i < a.attempt && wait < a.backoffCap; i++ {
		wait *= 2
		if wait > a.backoffCap {
			wait = a.backoffCap
		}
	}
	return wait
}

func sleepWithContext(ctx context.Context, wait time.Duration) bool {
	if wait <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
