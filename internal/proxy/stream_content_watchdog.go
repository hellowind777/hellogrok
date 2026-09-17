package proxy

import (
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

const (
	// grokBuildDefaultIdleTimeoutSecs mirrors the shell's fallback when
	// neither the model nor [models] configures inference_idle_timeout_secs
	// (xai-grok-shell mvp_agent::resolve_inference_idle_timeout_secs).
	grokBuildDefaultIdleTimeoutSecs uint64 = 600
	// contentWatchdogMargin keeps the watchdog ahead of Grok Build's own
	// content-progress timer so a stall surfaces as a retryable proxy stream
	// error instead of the client's non-retryable IdleTimeout.
	contentWatchdogMargin = 30 * time.Second
	// minContentWatchdogTimeout disables the watchdog for very small
	// configured timeouts: racing an explicit fast-fail choice is pointless.
	minContentWatchdogTimeout = 60 * time.Second
)

// contentWatchdog mirrors Grok Build's per-protocol "meaningful content"
// idle timer one safety margin earlier. Relays can hold a stream for many
// minutes with keepalive bytes that never reach the model's answer; Grok
// Build counts only real content (text, reasoning, tool deltas, terminal
// signals) and ends the turn with a non-retryable IdleTimeout. Firing first
// lets hellogrok emit a retryable stream error so the turn enters Grok
// Build's native retry budget instead of dying.
type contentWatchdog struct {
	timeout time.Duration
	onFire  func()
	mu      sync.Mutex
	timer   *time.Timer
	fired   bool
	stopped bool
}

func newContentWatchdog(route config.Route, onFire func()) *contentWatchdog {
	timeout := contentWatchdogTimeout(route)
	if timeout <= 0 || onFire == nil {
		return nil
	}
	watchdog := &contentWatchdog{timeout: timeout, onFire: onFire}
	watchdog.timer = time.AfterFunc(timeout, watchdog.fire)
	return watchdog
}

// contentWatchdogTimeout derives the effective client deadline from the
// route (already inheritance-resolved and, while hellogrok manages it, the
// materialized value) minus the safety margin.
func contentWatchdogTimeout(route config.Route) time.Duration {
	secs := route.InferenceIdleTimeoutSecs
	if !route.InferenceIdleTimeoutConfigured || secs == 0 {
		secs = grokBuildDefaultIdleTimeoutSecs
	}
	timeout := time.Duration(secs)*time.Second - contentWatchdogMargin
	if timeout < minContentWatchdogTimeout {
		return 0
	}
	return timeout
}

func (watchdog *contentWatchdog) fire() {
	watchdog.mu.Lock()
	if watchdog.stopped || watchdog.fired {
		watchdog.mu.Unlock()
		return
	}
	watchdog.fired = true
	watchdog.mu.Unlock()
	watchdog.onFire()
}

// content resets the timer; call only for frames Grok Build itself would
// count as meaningful content.
func (watchdog *contentWatchdog) content() {
	if watchdog == nil {
		return
	}
	watchdog.mu.Lock()
	defer watchdog.mu.Unlock()
	if watchdog.stopped || watchdog.fired {
		return
	}
	watchdog.timer.Reset(watchdog.timeout)
}

func (watchdog *contentWatchdog) stop() {
	if watchdog == nil {
		return
	}
	watchdog.mu.Lock()
	watchdog.stopped = true
	watchdog.mu.Unlock()
	watchdog.timer.Stop()
}

func (watchdog *contentWatchdog) stalled() bool {
	if watchdog == nil {
		return false
	}
	watchdog.mu.Lock()
	defer watchdog.mu.Unlock()
	return watchdog.fired
}

// observe resets the timer when the frame carries content Grok Build would
// count toward its own idle timer.
func (watchdog *contentWatchdog) observe(protocol wireProtocol, frame map[string]any) {
	if watchdog == nil || frame == nil {
		return
	}
	if downstreamFrameHasContent(protocol, frame) {
		watchdog.content()
	}
}

// downstreamFrameHasContent mirrors Grok Build's per-protocol meaningful
// content classification (xai-grok-sampler stream consumers). Unknown or
// liveness-only frames return false so the watchdog never outlasts the
// client's own timer on frames the client ignores.
func downstreamFrameHasContent(protocol wireProtocol, frame map[string]any) bool {
	switch protocol {
	case wireChatCompletions:
		return chatFrameHasContent(frame)
	case wireMessages:
		return messagesFrameHasContent(frame)
	case wireResponses:
		return responsesFrameHasContent(frame)
	}
	return false
}

// chatFrameHasContent mirrors the Chat Completions consumer: a set
// finish_reason, non-empty content or reasoning_content, or any tool_calls
// delta counts as progress. Usage-only chunks and empty deltas do not.
func chatFrameHasContent(frame map[string]any) bool {
	if frame["error"] != nil {
		return true
	}
	choices, _ := frame["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		if finish, ok := choice["finish_reason"]; ok && finish != nil {
			if value, _ := finish.(string); value != "" {
				return true
			}
		}
		delta, _ := choice["delta"].(map[string]any)
		if value, _ := delta["content"].(string); value != "" {
			return true
		}
		if value, _ := delta["reasoning_content"].(string); value != "" {
			return true
		}
		if calls, _ := delta["tool_calls"].([]any); len(calls) > 0 {
			return true
		}
	}
	return false
}

// messagesFrameHasContent mirrors the Messages consumer: every event except
// ping counts as progress.
func messagesFrameHasContent(frame map[string]any) bool {
	typ := stringValue(frame["type"])
	return typ != "" && typ != "ping"
}

// responsesFrameHasContent mirrors the Responses consumer: lifecycle
// bookkeeping events (created/in_progress/queued) and empty deltas do not
// count; substantive deltas, items, and terminal events do.
func responsesFrameHasContent(frame map[string]any) bool {
	switch stringValue(frame["type"]) {
	case "", "response.created", "response.in_progress", "response.queued":
		return false
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.function_call_arguments.delta", "response.mcp_call_arguments.delta",
		"response.code_interpreter_call_code.delta", "response.custom_tool_call_input.delta":
		return stringValue(frame["delta"]) != ""
	case "response.output_text.done":
		return stringValue(frame["text"]) != ""
	case "response.refusal.done":
		return stringValue(frame["refusal"]) != ""
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		return stringValue(frame["text"]) != ""
	case "response.function_call_arguments.done":
		return stringValue(frame["arguments"]) != "" || stringValue(frame["name"]) != ""
	case "response.mcp_call_arguments.done":
		return stringValue(frame["arguments"]) != ""
	case "response.code_interpreter_call_code.done":
		return stringValue(frame["code"]) != ""
	case "response.custom_tool_call_input.done":
		return stringValue(frame["input"]) != ""
	}
	return true
}
