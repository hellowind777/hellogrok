package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func newTestUsageGuard() *contextUsageGuard {
	guard := newContextUsageGuard(nil)
	guard.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	return guard
}

func coherentResult(input, output int64) canonicalResult {
	return canonicalResult{
		UsagePresent:       true,
		LiveContextPresent: true,
		InputTokens:        input,
		OutputTokens:       output,
		TotalTokens:        input + output,
	}
}

func TestContextUsageGuardAcceptsCoherentGrowth(t *testing.T) {
	guard := newTestUsageGuard()
	request := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: guard}

	first := coherentResult(450_000, 1_000)
	request.suppressInconsistentUsage("channel", &first)
	if !first.UsagePresent || !first.LiveContextPresent {
		t.Fatalf("first coherent report was suppressed: %#v", first)
	}

	request.GuardBytes = 1_840_000
	growth := coherentResult(460_000, 900)
	request.suppressInconsistentUsage("channel", &growth)
	if !growth.UsagePresent {
		t.Fatalf("growth proportional to the request body was suppressed: %#v", growth)
	}
}

func TestContextUsageGuardSuppressesDoubledReport(t *testing.T) {
	guard := newTestUsageGuard()
	request := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: guard}

	first := coherentResult(450_000, 1_000)
	request.suppressInconsistentUsage("channel", &first)

	doubled := coherentResult(909_000, 2_000)
	request.suppressInconsistentUsage("channel", &doubled)
	if doubled.UsagePresent || doubled.LiveContextPresent {
		t.Fatalf("doubled report reached Grok Build: %#v", doubled)
	}

	next := coherentResult(455_000, 500)
	request.suppressInconsistentUsage("channel", &next)
	if !next.UsagePresent {
		t.Fatalf("coherent report after a suppression was dropped: %#v", next)
	}
}

func TestContextUsageGuardRelearnsAfterContextRewrite(t *testing.T) {
	guard := newTestUsageGuard()
	request := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: guard}

	first := coherentResult(450_000, 1_000)
	request.suppressInconsistentUsage("channel", &first)

	request.GuardBytes = 105_000
	compacted := coherentResult(20_000, 500)
	request.suppressInconsistentUsage("channel", &compacted)
	if !compacted.UsagePresent {
		t.Fatalf("post-compaction report was suppressed: %#v", compacted)
	}

	doubled := coherentResult(41_000, 300)
	request.suppressInconsistentUsage("channel", &doubled)
	if doubled.UsagePresent {
		t.Fatalf("doubled report against the relearned baseline was accepted: %#v", doubled)
	}
}

func TestContextUsageGuardDisabledWithoutSessionOrGuard(t *testing.T) {
	guard := newTestUsageGuard()
	for _, request := range []facadeRequest{
		{GuardBytes: 1_800_000, UsageGuard: guard},
		{SessionKey: "session-1", GuardBytes: 1_800_000},
		{SessionKey: "session-1", UsageGuard: guard},
	} {
		result := coherentResult(909_000, 2_000)
		request.suppressInconsistentUsage("channel", &result)
		if !result.UsagePresent {
			t.Fatalf("guard acted without a complete scope: %#v", request)
		}
	}
}

func TestContextUsageGuardIsolatesSessionsAndChannels(t *testing.T) {
	guard := newTestUsageGuard()
	base := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: guard}
	first := coherentResult(450_000, 1_000)
	base.suppressInconsistentUsage("channel", &first)

	other := facadeRequest{SessionKey: "session-2", GuardBytes: 1_800_000, UsageGuard: guard}
	doubled := coherentResult(909_000, 2_000)
	other.suppressInconsistentUsage("channel", &doubled)
	if !doubled.UsagePresent {
		t.Fatalf("baseline leaked across sessions: %#v", doubled)
	}

	otherChannel := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: guard}
	doubledAgain := coherentResult(909_000, 2_000)
	otherChannel.suppressInconsistentUsage("other-channel", &doubledAgain)
	if !doubledAgain.UsagePresent {
		t.Fatalf("baseline leaked across channels: %#v", doubledAgain)
	}
}

func TestGuardNativeChatUsageDropsContradictoryChatUsage(t *testing.T) {
	server := New(log.New(io.Discard, "", 0))
	route := config.Route{ChannelID: "channel"}
	request := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: server.usageGuard}

	coherent := map[string]any{"usage": map[string]any{
		"prompt_tokens": 450_000, "completion_tokens": 1_000, "total_tokens": 451_000,
	}}
	server.guardNativeChatUsage(coherent, route, request)
	if coherent["usage"] == nil {
		t.Fatal("first coherent native chat usage was dropped")
	}

	doubled := map[string]any{"usage": map[string]any{
		"prompt_tokens": 909_000, "completion_tokens": 2_000, "total_tokens": 911_000,
	}}
	server.guardNativeChatUsage(doubled, route, request)
	if doubled["usage"] != nil {
		t.Fatalf("contradictory native chat usage reached Grok Build: %#v", doubled["usage"])
	}
}

func TestTokenizableBodyBytesExcludesDataURIs(t *testing.T) {
	blob := "data:image/png;base64," + strings.Repeat("A", 4096)
	body := []byte(`{"messages":[{"role":"user","content":"hi"},{"role":"user","content":"` + blob + `"}]}`)
	got := tokenizableBodyBytes(body)
	want := int64(len(body)) - int64(len(blob))
	if got != want {
		t.Fatalf("tokenizable bytes = %d, want %d", got, want)
	}
	if invalid := tokenizableBodyBytes([]byte(`{`)); invalid != 1 {
		t.Fatalf("undecodable body fell back to %d bytes, want raw length", invalid)
	}
}

func TestGuardResponsesUsageDropsContradictoryUsageInBothShapes(t *testing.T) {
	server := New(log.New(io.Discard, "", 0))
	route := config.Route{ChannelID: "channel"}
	request := facadeRequest{SessionKey: "session-1", GuardBytes: 1_800_000, UsageGuard: server.usageGuard}

	coherent := map[string]any{"usage": map[string]any{"input_tokens": 450_000, "output_tokens": 1_000}}
	server.guardResponsesUsage(coherent, route, request)
	if coherent["usage"] == nil {
		t.Fatal("first coherent native responses usage was dropped")
	}

	doubled := map[string]any{"response": map[string]any{"usage": map[string]any{"input_tokens": 909_000, "output_tokens": 2_000}}}
	server.guardResponsesUsage(doubled, route, request)
	inner, _ := doubled["response"].(map[string]any)
	if inner["usage"] != nil {
		t.Fatalf("contradictory envelope usage reached Grok Build: %#v", inner["usage"])
	}
}

func TestNativeResponsesStreamSuppressesContradictoryUsageEndToEnd(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		prompt := 1000 + calls*1000
		calls++
		_, _ = io.WriteString(w,
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"wire","output":[],"usage":{"input_tokens":`+
				strconv.Itoa(prompt)+`,"output_tokens":5,"total_tokens":`+strconv.Itoa(prompt+5)+`}}}`+"\n\n")
	}))
	defer upstream.Close()

	route := facadeRoute("usage-guard-resp", "responses", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	session := http.Header{"X-Grok-Session-ID": []string{"session-43"}}
	first, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", session)
	if status != http.StatusOK || !bytes.Contains(first, []byte(`"input_tokens":1000`)) {
		t.Fatalf("first call status=%d body=%s", status, first)
	}
	second, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", session)
	if status != http.StatusOK {
		t.Fatalf("second call status=%d body=%s", status, second)
	}
	if bytes.Contains(second, []byte(`"input_tokens":2000`)) {
		t.Fatalf("contradictory responses usage reached Grok Build: %s", second)
	}
}

func TestNativeChatStreamSuppressesContradictoryUsageEndToEnd(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		prompt := 1000 + calls*1000
		calls++
		_, _ = io.WriteString(w,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[{"index":0,"delta":{"content":"ok"}}]}`+"\n\n"+
				`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":`+
				strconv.Itoa(prompt)+`,"completion_tokens":5,"total_tokens":`+strconv.Itoa(prompt+5)+`}}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer upstream.Close()

	route := facadeRoute("usage-guard", "chat_completions", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	session := http.Header{"X-Grok-Session-ID": []string{"session-42"}}
	first, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireChatCompletions, nativeRequestBody("chat_completions", true), "", session)
	if status != http.StatusOK || !bytes.Contains(first, []byte(`"prompt_tokens":1000`)) || !bytes.Contains(first, []byte(`"content":"ok"`)) {
		t.Fatalf("first call status=%d body=%s", status, first)
	}
	second, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireChatCompletions, nativeRequestBody("chat_completions", true), "", session)
	if status != http.StatusOK {
		t.Fatalf("second call status=%d body=%s", status, second)
	}
	if bytes.Contains(second, []byte(`"prompt_tokens":2000`)) {
		t.Fatalf("contradictory usage reached Grok Build: %s", second)
	}
	if !bytes.Contains(second, []byte("ok")) {
		t.Fatalf("assistant content was lost while suppressing usage: %s", second)
	}
}
