package proxy

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// contextUsageGuard detects provider usage reports that contradict the live
// request context. Relays occasionally double-count the cached prefix or
// report session-cumulative prompt tokens; Grok Build feeds the reported
// total straight into its context meter and auto-compact threshold, so one
// inflated report fires compaction far above the configured percentage. The
// guard learns a bytes-per-token ratio per conversation from the first
// coherent report and suppresses later reports whose prompt count overshoots
// what the request body can hold. Suppressed reports are dropped entirely,
// leaving Grok Build on its own byte-based estimate until the provider
// reports coherently again.
type contextUsageGuard struct {
	mu      sync.Mutex
	entries map[string]contextUsageBaseline
	logf    func(format string, args ...any)
	now     func() time.Time
}

type contextUsageBaseline struct {
	bytesPerToken float64
	bodyBytes     int64
	seenAt        time.Time
	suppressed    int
}

const (
	// guardOverReportRatio is the most a reported prompt count may exceed the
	// body-derived expectation before the report is treated as contradictory.
	guardOverReportRatio = 1.25
	// guardRewriteShrink marks a request body shrink that means the context
	// was rewritten (compaction, rewind, resume), invalidating the baseline.
	guardRewriteShrink = 0.75
	guardEntryTTL      = 24 * time.Hour
	guardMaxEntries    = 4096
)

func newContextUsageGuard(logf func(format string, args ...any)) *contextUsageGuard {
	return &contextUsageGuard{
		entries: map[string]contextUsageBaseline{},
		logf:    logf,
		now:     time.Now,
	}
}

// observe reports whether a usage measurement may reach Grok Build. channel
// and session scope the baseline to one conversation; an empty session or
// non-positive measurement disables the check for this report.
func (g *contextUsageGuard) observe(channel, session string, bodyBytes, promptTokens int64) bool {
	if g == nil || channel == "" || session == "" || bodyBytes <= 0 || promptTokens <= 0 {
		return true
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(now)
	key := channel + "\x00" + session
	reported := float64(promptTokens)
	entry, ok := g.entries[key]
	if !ok || float64(bodyBytes) < guardRewriteShrink*float64(entry.bodyBytes) {
		g.entries[key] = contextUsageBaseline{
			bytesPerToken: float64(bodyBytes) / reported,
			bodyBytes:     bodyBytes,
			seenAt:        now,
		}
		return true
	}
	expected := float64(bodyBytes) / entry.bytesPerToken
	if reported > guardOverReportRatio*expected {
		entry.suppressed++
		entry.seenAt = now
		g.entries[key] = entry
		if g.logf != nil {
			g.logf("UP channel=%s usage suppressed: session=%s reported prompt=%d exceeds the request context estimate=%d; Grok Build falls back to its local estimate",
				channel, session, promptTokens, int64(expected))
		}
		return false
	}
	entry.bytesPerToken = float64(bodyBytes) / reported
	entry.bodyBytes = bodyBytes
	entry.seenAt = now
	entry.suppressed = 0
	g.entries[key] = entry
	return true
}

func (g *contextUsageGuard) pruneLocked(now time.Time) {
	if len(g.entries) < guardMaxEntries/2 {
		return
	}
	for key, entry := range g.entries {
		if now.Sub(entry.seenAt) > guardEntryTTL {
			delete(g.entries, key)
		}
	}
}

// suppressInconsistentUsage drops translated usage reports the guard rejects
// before they are projected into the Responses envelope.
func (request facadeRequest) suppressInconsistentUsage(channel string, result *canonicalResult) {
	if request.UsageGuard == nil || result == nil || !result.UsagePresent {
		return
	}
	if !request.UsageGuard.observe(channel, request.SessionKey, request.GuardBytes, result.InputTokens) {
		result.UsagePresent = false
		result.LiveContextPresent = false
	}
}

// guardNativeChatUsage drops native Chat usage the guard rejects before Grok
// Build's Chat consumer reads total_tokens as the live context length.
func (s *Server) guardNativeChatUsage(root map[string]any, route config.Route, request facadeRequest) {
	if request.UsageGuard == nil || root == nil {
		return
	}
	usage, _ := root["usage"].(map[string]any)
	if usage == nil {
		return
	}
	prompt, _, _ := firstCanonicalToken(usage, "prompt_tokens", "input_tokens")
	if request.UsageGuard.observe(route.ChannelID, request.SessionKey, request.GuardBytes, prompt) {
		return
	}
	root["usage"] = nil
}

// guardResponsesUsage drops native Responses usage the guard rejects before
// Grok Build's Responses consumer rewrites total_tokens from it. Accepts both
// a bare response object and an SSE event envelope wrapping one.
func (s *Server) guardResponsesUsage(root map[string]any, route config.Route, request facadeRequest) {
	if request.UsageGuard == nil || root == nil {
		return
	}
	envelope := root
	if inner, ok := root["response"].(map[string]any); ok {
		envelope = inner
	}
	usage, _ := envelope["usage"].(map[string]any)
	if usage == nil {
		return
	}
	prompt, _, _ := firstCanonicalToken(usage, "input_tokens")
	if request.UsageGuard.observe(route.ChannelID, request.SessionKey, request.GuardBytes, prompt) {
		return
	}
	envelope["usage"] = nil
}

// tokenizableBodyBytes approximates the byte footprint a provider tokenizes:
// the encoded body minus embedded base64 data URIs, which carry image bytes
// but a nearly constant token cost. Structural JSON overhead stays included
// because the guard only compares ratios across calls of one conversation.
func tokenizableBodyBytes(body []byte) int64 {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return int64(len(body))
	}
	return int64(len(body)) - dataURIBytes(root)
}

func dataURIBytes(value any) int64 {
	switch typed := value.(type) {
	case string:
		if strings.HasPrefix(typed, "data:") && len(typed) > 256 {
			return int64(len(typed))
		}
		return 0
	case []any:
		var total int64
		for _, item := range typed {
			total += dataURIBytes(item)
		}
		return total
	case map[string]any:
		var total int64
		for _, item := range typed {
			total += dataURIBytes(item)
		}
		return total
	}
	return 0
}
