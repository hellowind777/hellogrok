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
const (
	defaultAbsorbRetryMaxSecs        = 90
	defaultAbsorbRetryBackoffCapSecs = 30
	absorbMinBackoff                 = 2 * time.Second
	absorbMaxUpstreamRetryAfter      = 60 * time.Second
	// synthesizedRetryAfterSeconds paces Grok Build's own retries when an
	// upstream busy/overloaded error carried no Retry-After of its own. Grok
	// Build caps a Retry-After hint at 30 seconds, so larger values gain
	// nothing.
	synthesizedRetryAfterSeconds = 30
)

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

// absorbState tracks one request's wait-and-retry window. Budget and backoff
// come from the route configuration; now and sleep are injectable for tests.
type absorbState struct {
	budget     time.Duration // 0 = disabled
	backoffCap time.Duration
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
	}
}

// elapsed reports how much of the budget this request has already waited.
func (a *absorbState) elapsed() time.Duration {
	if a.now == nil || a.started.IsZero() {
		return 0
	}
	return a.now().Sub(a.started)
}

// wait pauses for the next backoff interval or the remaining budget,
// whichever is smaller. retryAfterSeconds comes from the upstream error
// response when present (capped); zero uses exponential backoff. It returns
// false when the budget is exhausted or the context is done, meaning the
// caller must pass the failure through to the client.
func (a *absorbState) wait(ctx context.Context, retryAfter int) bool {
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
	wait := a.nextWait(retryAfter)
	if remaining := a.budget - elapsed; wait > remaining {
		wait = remaining
	}
	a.attempt++
	a.lastWait = wait
	return a.sleep(ctx, wait)
}

func (a *absorbState) nextWait(retryAfter int) time.Duration {
	if retryAfter > 0 {
		wait := time.Duration(retryAfter) * time.Second
		if wait > absorbMaxUpstreamRetryAfter {
			return absorbMaxUpstreamRetryAfter
		}
		return wait
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
