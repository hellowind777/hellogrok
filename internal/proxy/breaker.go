package proxy

import (
	"sync"
	"time"
)

// Grok Build's sampler retries retryable statuses (429, 5xx) up to 15 times
// with exponential backoff capped at 30s — about 5.5 minutes per turn. When a
// third-party channel goes down (e.g. a Cloudflare 502 from the relay's
// origin), every one of those retries fails and the user is stuck in the
// "retrying" phase until the turn dies; the next turn repeats the storm.
//
// The breaker makes that failure fail fast: after a short streak of retryable
// upstream failures the proxy answers immediately with a non-retryable
// response (`X-Should-Retry: false`), which Grok Build treats as fatal. After
// a cooldown one probe request is allowed through; success closes the breaker,
// failure re-arms it.
const (
	// circuitOpenThreshold consecutive retryable upstream failures open the
	// breaker. Below this, transient blips still get Grok Build's own retries.
	circuitOpenThreshold = 4
	// circuitOpenCooldown is how long the breaker rejects requests before
	// allowing a single probe attempt.
	circuitOpenCooldown = 90 * time.Second
)

// circuitOpenErrorMessage is shown to the user in Grok Build's error UI when
// the breaker rejects a request.
const circuitOpenErrorMessage = "上游渠道连续失败，hellogrok 已暂时熔断停止重试；约 90 秒后自动放行一次探测恢复，或切换到其它渠道"

type breakerTransition int

const (
	breakerNone breakerTransition = iota
	breakerOpened
	breakerProbeFailed
)

type upstreamBreaker struct {
	failures int
	openedAt time.Time // zero = closed
	probing  bool
}

// breakerStore holds one circuit breaker per channel. Failures are only
// counted when Grok Build would actually retry them (effective retryable
// disposition); any non-5xx response proves the channel is responsive and
// closes the breaker.
type breakerStore struct {
	mu       sync.Mutex
	now      func() time.Time
	channels map[string]*upstreamBreaker
}

func newBreakerStore() *breakerStore {
	return &breakerStore{
		now:      time.Now,
		channels: make(map[string]*upstreamBreaker),
	}
}

// reset clears all breaker state. Called when the proxy starts a fresh
// upstream lifecycle (Enable).
func (b *breakerStore) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.channels = make(map[string]*upstreamBreaker)
}

// allow reports whether a request for the channel may be sent upstream. An
// open breaker rejects immediately; after the cooldown a single probe is
// allowed through.
func (b *breakerStore) allow(channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.channels[channel]
	if state == nil || state.openedAt.IsZero() {
		return true
	}
	if b.now().Sub(state.openedAt) < circuitOpenCooldown {
		return false
	}
	if state.probing {
		return false
	}
	state.probing = true
	return true
}

// recordSuccess closes the breaker for the channel.
func (b *breakerStore) recordSuccess(channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, open := b.channels[channel]
	delete(b.channels, channel)
	return open
}

// recordFailure counts a retryable upstream failure and reports the transition
// for logging.
func (b *breakerStore) recordFailure(channel string) breakerTransition {
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.channels[channel]
	if state == nil {
		state = &upstreamBreaker{}
		b.channels[channel] = state
	}
	if !state.openedAt.IsZero() {
		// A failed probe (or any stray failure while open) re-arms the cooldown.
		state.openedAt = b.now()
		state.probing = false
		return breakerProbeFailed
	}
	state.failures++
	if state.failures >= circuitOpenThreshold {
		state.openedAt = b.now()
		state.failures = 0
		return breakerOpened
	}
	return breakerNone
}
