package proxy

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Grok Build's own retry loop (up to 15 attempts, roughly 5.5 minutes per
// turn) is the user-visible retry budget. hellogrok never shortens it for
// soft failures: transient upstream errors (busy 503, 429, timeouts) are
// either absorbed by the proxy's bounded wait-and-retry window or pass
// through as retryable so the client keeps its full budget.
//
// The only failure mode where that budget is pure waste is a dead channel:
// dial-level failures (connection refused, DNS errors, TLS handshakes) can
// never succeed on retry. The opt-in dead-channel breaker
// (`dead_channel_fail_fast`) fast-fails after a streak of those so the turn
// ends quickly instead of burning the whole budget. Any upstream response,
// whatever its status, proves the channel is reachable and closes the
// breaker. The breaker is fully transparent when the channel did not opt in.
const (
	// defaultDeadChannelFailThreshold consecutive dial-level failures open
	// the opt-in dead-channel breaker.
	defaultDeadChannelFailThreshold = 6
	// deadChannelCooldown is how long the open breaker rejects requests
	// before allowing a single probe attempt.
	deadChannelCooldown = 5 * time.Minute
	// deadChannelProbeLease caps how long a probe may stay in flight before it
	// is presumed lost. A probe that ends without a dial-level failure or an
	// upstream response (for example the client disconnected mid-probe, or the
	// response headers timed out) never calls recordSuccess/recordFailure;
	// without a lease that would leave probing latched and the channel
	// permanently fast-failing.
	deadChannelProbeLease = 2 * time.Minute
)

// deadChannelErrorMessage is reported when the opt-in breaker rejects a
// request. Grok Build's pager replaces every 5xx detail with its own copy,
// so this text primarily serves proxy logs and direct API consumers.
const deadChannelErrorMessage = "upstream channel unreachable; consecutive dial-level failures exceeded the dead-channel threshold"

type breakerTransition int

const (
	breakerNone breakerTransition = iota
	breakerOpened
	breakerProbeFailed
)

type upstreamBreaker struct {
	failures     int
	openedAt     time.Time // zero = closed
	probing      bool
	probeStarted time.Time // when the in-flight probe was released
}

// breakerParams carries a channel's opt-in policy. When Enabled is false the
// breaker is fully transparent: allow always succeeds and failures are never
// counted.
type breakerParams struct {
	Enabled   bool
	Threshold int
}

func (p breakerParams) threshold() int {
	if p.Threshold <= 0 {
		return defaultDeadChannelFailThreshold
	}
	return p.Threshold
}

func deadChannelBreakerParams(routeDeadChannelFailFast bool, routeDeadChannelFailThreshold uint64) breakerParams {
	return breakerParams{
		Enabled:   routeDeadChannelFailFast,
		Threshold: int(routeDeadChannelFailThreshold),
	}
}

// isHardUpstreamError reports whether an upstream request error is a
// dial-level failure: the channel is unreachable rather than merely busy or
// slow. Only these errors feed the dead-channel breaker. Timeouts, mid-body
// resets, and HTTP error statuses are soft and never counted.
func isHardUpstreamError(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var authorityErr x509.UnknownAuthorityError
	if errors.As(err, &authorityErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	// TLS handshake failures never succeed on retry, so they count toward
	// the dial-level breaker even though they surface after a connection
	// was established.
	var recErr tls.RecordHeaderError
	if errors.As(err, &recErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Op == "dial" && !opErr.Timeout() {
			return true
		}
		// crypto/tls wraps a received TLS alert as an OpError whose Op is
		// the alert origin: "remote error" for a remote alert and "local
		// error" for one we sent ourselves.
		if (opErr.Op == "remote error" || opErr.Op == "local error") && !opErr.Timeout() {
			return true
		}
	}
	// net/http produces this fixed string when an HTTP server answers a TLS
	// handshake. The Go versions this project targets export no error type
	// for it, so only the message can be matched.
	if strings.Contains(err.Error(), "server gave HTTP response to HTTPS client") {
		return true
	}
	return false
}

// breakerStore holds one dead-channel breaker per channel. Failures are only
// counted for channels that opted in via dead_channel_fail_fast, and only
// for dial-level failures; any upstream response proves the channel is
// reachable and closes the breaker.
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
// open dead-channel breaker rejects immediately; after the cooldown a single
// probe is allowed through. Channels that did not opt in always pass.
func (b *breakerStore) allow(channel string, params breakerParams) bool {
	if !params.Enabled {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	state := b.channels[channel]
	if state == nil || state.openedAt.IsZero() {
		return true
	}
	if b.now().Sub(state.openedAt) < deadChannelCooldown {
		return false
	}
	if state.probing {
		if b.now().Sub(state.probeStarted) < deadChannelProbeLease {
			return false
		}
		// The previous probe never reported a result (non-dial failure or a
		// lost client). Release it and allow a fresh probe.
		state.probing = false
	}
	state.probing = true
	state.probeStarted = b.now()
	return true
}

// recordSuccess closes the breaker for the channel. Any upstream response of
// any status proves the channel is reachable.
func (b *breakerStore) recordSuccess(channel string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, open := b.channels[channel]
	delete(b.channels, channel)
	return open
}

// recordFailure counts a dial-level failure and reports the transition for
// logging. Channels that did not opt in are never counted.
func (b *breakerStore) recordFailure(channel string, params breakerParams) breakerTransition {
	if !params.Enabled {
		return breakerNone
	}
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
	if state.failures >= params.threshold() {
		state.openedAt = b.now()
		state.failures = 0
		return breakerOpened
	}
	return breakerNone
}
