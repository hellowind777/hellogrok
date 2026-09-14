package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

// Relays (newapi/sub2api style) sometimes answer a failure with HTTP 200 plus
// an error envelope. A transient envelope must enter the absorb layer and
// surface the recovered success instead of dying as an opaque envelope
// rejection.
func TestWrapped2xxTransientAbsorbedThenRecovers(t *testing.T) {
	var served int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Header().Set("Content-Type", "application/json")
		if served <= 2 {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"rate limit exceeded, retry later"}}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, nativeSuccessBody("responses", "wire", "OK"))
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	data, status, _ := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want the wrapped transient errors absorbed and the recovery surfaced)", status, data)
	}
	if got := *calls; got != 3 {
		t.Fatalf("upstream calls=%d want 3 (two wrapped errors plus the recovery)", got)
	}
}

// A deterministic wrapped error passes through with the provider's
// explanation and a terminal disposition: one failure, no retry loop.
func TestWrapped2xxDeterministicPassthroughKeepsProviderMessage(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid api key for this channel"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 0
	})

	data, status, header := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s (want the deterministic wrapped error as 400)", status, data)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("deterministic wrapped error must stay terminal, got %q", header.Get("X-Should-Retry"))
	}
	if !containsJSONType(data, "authentication_error") {
		t.Fatalf("provider explanation lost: %s", data)
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1 with the absorb layer disabled", got)
	}
}

// A Responses terminal body carries its own top-level error member; it must
// keep the native failed-response path, not the wrapped-error surrogate.
func TestWrapped2xxLeavesResponsesFailedOnNativePath(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"resp_9","object":"response","status":"failed","model":"wire","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"error":{"type":"server_error","message":"inference failed"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)

	data, status, _ := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want the native failed response delivered)", status, data)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if root["status"] != "failed" || root["id"] != "resp_9" {
		t.Fatalf("native failed body was rewritten: %s", data)
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}
}

// A Cloudflare shield challenge in front of the relay clears on its own: the
// absorb layer hides it and the recovered success surfaces.
func TestCloudflareChallengeAbsorbedThenRecovers(t *testing.T) {
	var served int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		if served <= 2 {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("cf-mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<html><head><title>Just a moment...</title></head></html>`)
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
		t.Fatalf("status=%d body=%s (want the challenge absorbed and the recovery surfaced)", status, data)
	}
	if got := *calls; got != 3 {
		t.Fatalf("upstream calls=%d want 3 (two challenges plus the recovery)", got)
	}
}

// When a challenge outlasts the absorb window it passes through as a
// retryable 503: 403 would classify terminal in Grok Build and kill the turn
// on a self-clearing shield.
func TestCloudflareChallengePassthroughIsRetryable503(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("server", "cloudflare")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `<html><head><title>Just a moment...</title></head><body>cf-chl-captcha</body></html>`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 0
	})

	data, status, header := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s (want the challenge as 503)", status, data)
	}
	if header.Get("X-Should-Retry") != "true" {
		t.Fatalf("challenge passthrough must stay retryable for Grok Build, got %q", header.Get("X-Should-Retry"))
	}
	if header.Get("Retry-After") != "30" {
		t.Fatalf("challenge passthrough must carry the synthesized Retry-After, got %q", header.Get("Retry-After"))
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1 with the absorb layer disabled", got)
	}
}

// A genuine origin 403 (permission) must not be mistaken for a challenge:
// no cf-mitigated header, no Cloudflare server header.
func TestOrigin403IsNotMistakenForChallenge(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"permission_error","message":"this key may not use this model"}}`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 0
	})

	data, status, header := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusForbidden {
		t.Fatalf("status=%d body=%s (want the origin 403 untouched)", status, data)
	}
	if header.Get("X-Should-Retry") != "false" {
		t.Fatalf("origin 403 must stay terminal, got %q", header.Get("X-Should-Retry"))
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}
}

// Relays that omit Responses envelope bookkeeping on a complete terminal
// body get the missing markers synthesized instead of a 502 rejection.
func TestResponsesEnvelopeBookkeepingSynthesized(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"model":"wire","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	server, calls := newBreakerTestServer(t, handler)

	data, status, _ := postFacadeResponse(t, server, "breaker-channel", nativeRequestBody("responses", false), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want the synthesized envelope delivered as success)", status, data)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	if root["object"] != "response" || root["status"] != "completed" || root["id"] == "" {
		t.Fatalf("envelope bookkeeping not synthesized: %s", data)
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}
}

func TestAssembleResponsesStreamTerminal(t *testing.T) {
	if _, _, ok := assembleResponsesStreamTerminal(map[string]any{
		"id": "resp_1", "object": "response", "status": "in_progress", "output": []any{},
	}, nil, ""); ok {
		t.Fatal("in-progress snapshot with no output must not look terminal")
	}
	eventType, body, ok := assembleResponsesStreamTerminal(map[string]any{
		"id": "resp_1", "object": "response", "status": "in_progress", "output": []any{},
	}, map[int]map[string]any{
		0: {"type": "message", "id": "msg_1", "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}},
	}, "")
	if !ok || eventType != "response.completed" || body["status"] != "completed" {
		t.Fatalf("output items should complete the stream: ok=%t type=%s body=%#v", ok, eventType, body)
	}
	eventType, body, ok = assembleResponsesStreamTerminal(map[string]any{
		"id": "resp_1", "object": "response", "status": "completed", "output": []any{},
	}, nil, "")
	if !ok || eventType != "response.completed" {
		t.Fatalf("explicit completed status should be terminal: ok=%t type=%s body=%#v", ok, eventType, body)
	}
}

// A relay reusing one tool_call ID across calls in a non-streaming Chat
// response is the same repairable defect the streaming rectifier tolerates:
// remap, do not reject.
func TestNativeChatDuplicateCallIDsRemapped(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"wire","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_dup","type":"function","function":{"name":"list_dir","arguments":"{}"}},{"index":1,"id":"call_dup","type":"function","function":{"name":"read_file","arguments":"{}"}}]}}]}`)
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.APIBackend = "chat_completions"
		route.SupportsBackendSearch = false
	})

	data, status, _ := postFacadeProtocol(t, server, "breaker-channel", wireChatCompletions, nativeRequestBody("chat_completions", false), "", nil)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want the duplicate call IDs remapped, not rejected)", status, data)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	choices, _ := root["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%d want 1: %s", len(choices), data)
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	calls_, _ := message["tool_calls"].([]any)
	if len(calls_) != 2 {
		t.Fatalf("tool_calls=%d want 2: %s", len(calls_), data)
	}
	first, _ := calls_[0].(map[string]any)
	second, _ := calls_[1].(map[string]any)
	idA, idB := first["id"].(string), second["id"].(string)
	if idA == "" || idB == "" || idA == idB {
		t.Fatalf("call IDs not unique after delivery: %q %q", idA, idB)
	}
	if got := *calls; got != 1 {
		t.Fatalf("upstream calls=%d want 1", got)
	}
}
