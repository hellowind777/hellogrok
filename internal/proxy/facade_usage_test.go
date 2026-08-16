package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestTranslatedUsageAbsenceDoesNotInventZero(t *testing.T) {
	messagesBodies := []string{
		`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":"end_turn"}`,
		`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":"end_turn","usage":null}`,
		`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":"end_turn","usage":{}}`,
	}
	for _, body := range messagesBodies {
		result, err := canonicalFromMessages([]byte(body), false, "")
		if err != nil {
			t.Fatal(err)
		}
		if result.UsagePresent || canonicalResponse(config.Route{}, facadeRequest{}, result)["usage"] != nil {
			t.Fatalf("missing Messages usage became a zero measurement: %#v", result)
		}
	}

	chatBodies := []string{
		`{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		`{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":null}`,
		`{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`,
	}
	for _, body := range chatBodies {
		result, err := canonicalFromChat([]byte(body), false, "")
		if err != nil {
			t.Fatal(err)
		}
		if result.UsagePresent || canonicalResponse(config.Route{}, facadeRequest{}, result)["usage"] != nil {
			t.Fatalf("missing Chat usage became a zero measurement: %#v", result)
		}
	}
}

func TestTranslatedUsageRejectsAllZeroPlaceholder(t *testing.T) {
	for _, apply := range []func(*canonicalResult, any){applyMessagesUsage, applyChatUsage} {
		result := canonicalResult{}
		apply(&result, map[string]any{
			"input_tokens":      0,
			"output_tokens":     0,
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		})
		if result.UsagePresent || canonicalResponse(config.Route{}, facadeRequest{}, result)["usage"] != nil {
			t.Fatalf("all-zero placeholder became a real measurement: %#v", result)
		}
	}
}

func TestMessagesUsageIncludesCacheCreationAndReadTokens(t *testing.T) {
	body := []byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":5}}`)
	result, err := canonicalFromMessages(body, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.UsagePresent || !result.LiveContextPresent || result.InputTokens != 60 || result.CachedTokens != 20 ||
		result.OutputTokens != 5 || result.TotalTokens != 65 {
		t.Fatalf("Messages usage was miscounted: %#v", result)
	}
}

func TestChatUsagePreservesProviderTotalCacheAndReasoning(t *testing.T) {
	body := []byte(`{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":20,"total_tokens":123,"prompt_cache_hit_tokens":10,"prompt_cache_miss_tokens":40,"completion_tokens_details":{"reasoning_tokens":7}}}`)
	result, err := canonicalFromChat(body, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.UsagePresent || !result.LiveContextPresent || result.InputTokens != 50 || result.OutputTokens != 20 ||
		result.TotalTokens != 123 || result.CachedTokens != 10 || result.ReasoningTokens != 7 {
		t.Fatalf("Chat usage was miscounted: %#v", result)
	}
	usage := canonicalResponse(config.Route{}, facadeRequest{}, result)["usage"].(map[string]any)
	contextDetails := usage["context_details"].(map[string]any)
	if usage["total_tokens"] != int64(123) || contextDetails["input_tokens"] != int64(50) ||
		contextDetails["output_tokens"] != int64(20) {
		t.Fatalf("provider total and live context were not kept distinct: %#v", usage)
	}

	withoutPrompt := canonicalResult{}
	applyChatUsage(&withoutPrompt, map[string]any{
		"prompt_cache_hit_tokens":  11,
		"prompt_cache_miss_tokens": 19,
		"completion_tokens":        3,
	})
	if !withoutPrompt.LiveContextPresent || withoutPrompt.InputTokens != 30 || withoutPrompt.TotalTokens != 33 || withoutPrompt.CachedTokens != 11 {
		t.Fatalf("Chat cache-only input usage was miscounted: %#v", withoutPrompt)
	}
}

func TestTranslatedUsageRejectsPartialAndInvalidMeasurements(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		usage    map[string]any
	}{
		{name: "messages input only", protocol: "messages", usage: map[string]any{"input_tokens": 3}},
		{name: "messages negative output", protocol: "messages", usage: map[string]any{"input_tokens": 3, "output_tokens": -1}},
		{name: "messages total only", protocol: "messages", usage: map[string]any{"total_tokens": 7}},
		{name: "messages input and total only", protocol: "messages", usage: map[string]any{"input_tokens": 3, "total_tokens": 7}},
		{name: "chat output only", protocol: "chat", usage: map[string]any{"completion_tokens": 3}},
		{name: "chat total only", protocol: "chat", usage: map[string]any{"total_tokens": 7}},
		{name: "chat prompt and total only", protocol: "chat", usage: map[string]any{"prompt_tokens": 3, "total_tokens": 7}},
		{name: "chat fractional input", protocol: "chat", usage: map[string]any{"prompt_tokens": 1.5, "completion_tokens": 3}},
		{name: "chat invalid detail", protocol: "chat", usage: map[string]any{
			"prompt_tokens": 1, "completion_tokens": 3,
			"completion_tokens_details": map[string]any{"reasoning_tokens": -1},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var result canonicalResult
			if test.protocol == "messages" {
				applyMessagesUsage(&result, test.usage)
			} else {
				applyChatUsage(&result, test.usage)
			}
			if result.UsagePresent {
				t.Fatalf("untrustworthy translated usage was accepted: %#v", result)
			}
		})
	}
}

func TestTranslatedUsageDoesNotEmitOverflowingLiveContext(t *testing.T) {
	for _, apply := range []func(*canonicalResult, any){applyMessagesUsage, applyChatUsage} {
		var result canonicalResult
		apply(&result, map[string]any{
			"input_tokens":      maxCanonicalTokenCount,
			"prompt_tokens":     maxCanonicalTokenCount,
			"output_tokens":     1,
			"completion_tokens": 1,
			"total_tokens":      maxCanonicalTokenCount,
		})
		if !result.UsagePresent || result.LiveContextPresent {
			t.Fatalf("overflowing live context was accepted or billing usage lost: %#v", result)
		}
		usage := canonicalResponse(config.Route{}, facadeRequest{}, result)["usage"].(map[string]any)
		if _, exists := usage["context_details"]; exists {
			t.Fatalf("overflowing live context was emitted: %#v", usage)
		}
	}
}

func TestChatFinishReasonsMapToResponsesTerminalStates(t *testing.T) {
	tests := []struct {
		finish     string
		status     string
		incomplete string
		code       string
	}{
		{"length", "incomplete", "max_output_tokens", ""},
		{"content_filter", "incomplete", "content_filter", ""},
		{"insufficient_system_resource", "failed", "", "insufficient_system_resource"},
		{"stop", "completed", "", ""},
	}
	for _, test := range tests {
		t.Run(test.finish, func(t *testing.T) {
			result := canonicalResult{}
			applyChatFinishReason(&result, test.finish)
			response := canonicalResponse(config.Route{}, facadeRequest{}, result)
			if response["status"] != test.status || result.IncompleteReason != test.incomplete || result.FailureCode != test.code {
				t.Fatalf("finish=%q result=%#v response=%#v", test.finish, result, response)
			}
		})
	}
}

func TestTranslatedStreamingUsageUsesTerminalUsageBlocks(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		body    string
		want    []string
	}{
		{
			name: "messages cache accounting", backend: "messages",
			body: `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			want: []string{`"input_tokens":60`, `"output_tokens":5`, `"total_tokens":65`, `"cached_tokens":20`, `"context_details":{"input_tokens":60,"output_tokens":5}`},
		},
		{
			name: "chat empty choices usage block", backend: "chat_completions",
			body: `data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":null}` + "\n\n" +
				`data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":12,"prompt_cache_hit_tokens":2,"completion_tokens_details":{"reasoning_tokens":1}}}` + "\n\n" +
				`data: [DONE]` + "\n\n",
			want: []string{`"input_tokens":7`, `"output_tokens":3`, `"total_tokens":12`, `"cached_tokens":2`, `"reasoning_tokens":1`, `"context_details":{"input_tokens":7,"output_tokens":3}`},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()

			route := facadeRoute("usage", test.backend, "wire", "key", upstream.URL)
			route.SupportsBackendSearch = true
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", true), "")
			if status != http.StatusOK || !bytes.Contains(data, []byte("response.completed")) {
				t.Fatalf("status=%d body=%s", status, data)
			}
			for _, want := range test.want {
				if !bytes.Contains(data, []byte(want)) {
					t.Fatalf("terminal usage missing %s: %s", want, data)
				}
			}
		})
	}
}

func TestTranslatedChatStreamingUsageDoesNotMergeSeparateSnapshots(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":80}}`+"\n\n"+
				`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[],"usage":{"completion_tokens":5,"total_tokens":85}}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer upstream.Close()

	route := facadeRoute("usage-snapshot", "chat_completions", "wire", "key", upstream.URL)
	route.SupportsBackendSearch = true
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status, _ := postFacadeProtocol(
		t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", nil,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var terminal map[string]any
	if err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
		root, err := decodeJSONMap(payload)
		if err == nil && root["type"] == "response.completed" {
			terminal, _ = root["response"].(map[string]any)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if terminal == nil || terminal["usage"] != nil {
		t.Fatalf("separate Chat usage snapshots were merged: %#v", terminal)
	}
}

func TestTranslatedChatStreamingUsageKeepsLastCompleteSnapshot(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\n"+
				`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[],"usage":{}}`+"\n\n"+
				`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[],"usage":{"total_tokens":100}}`+"\n\n"+
				`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`+"\n\n"+
				"data: [DONE]\n\n")
	}))
	defer upstream.Close()

	route := facadeRoute("usage-last-complete", "chat_completions", "wire", "key", upstream.URL)
	route.SupportsBackendSearch = true
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status, _ := postFacadeProtocol(
		t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", nil,
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	var terminal map[string]any
	if err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
		root, err := decodeJSONMap(payload)
		if err == nil && root["type"] == "response.completed" {
			terminal, _ = root["response"].(map[string]any)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	usage, _ := terminal["usage"].(map[string]any)
	context, _ := usage["context_details"].(map[string]any)
	if numberInt(usage["total_tokens"]) != 12 || numberInt(context["input_tokens"]) != 8 ||
		numberInt(context["output_tokens"]) != 4 {
		t.Fatalf("last complete Chat usage did not win: %#v", terminal)
	}
}

func TestTranslatedMessagesStreamingUsageUsesProtocolPhases(t *testing.T) {
	tests := []struct {
		name       string
		startUsage string
		deltas     string
		wantTotal  int
		wantNull   bool
	}{
		{
			name:       "partial tail cannot erase start plus output",
			startUsage: `{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":0,"total_tokens":60}`,
			deltas: `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"total_tokens":999}}` + "\n\n",
			wantTotal: 65,
		},
		{
			name:       "missing start input is not fabricated",
			startUsage: `{"output_tokens":0}`,
			deltas:     `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n",
			wantNull:   true,
		},
		{
			name:       "later complete snapshot wins",
			startUsage: `{"input_tokens":10,"output_tokens":0}`,
			deltas: `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":20,"cache_read_input_tokens":3,"output_tokens":4}}` + "\n\n",
			wantTotal: 27,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":`+test.startUsage+`}}`+"\n\n"+
						test.deltas+`data: {"type":"message_stop"}`+"\n\n")
			}))
			defer upstream.Close()

			route := facadeRoute("messages-usage-phases", "messages", "wire", "key", upstream.URL)
			route.SupportsBackendSearch = true
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status, _ := postFacadeProtocol(
				t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", nil,
			)
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			var terminal map[string]any
			if err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
				root, err := decodeJSONMap(payload)
				if err == nil && root["type"] == "response.completed" {
					terminal, _ = root["response"].(map[string]any)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if test.wantNull {
				if terminal == nil || terminal["usage"] != nil {
					t.Fatalf("unprovable Messages usage was emitted: %#v", terminal)
				}
				return
			}
			usage, _ := terminal["usage"].(map[string]any)
			if numberInt(usage["total_tokens"]) != test.wantTotal {
				t.Fatalf("Messages phase usage=%#v want total=%d", usage, test.wantTotal)
			}
		})
	}
}

func TestGrokBuildNativeUsageCompatibilityMatrix(t *testing.T) {
	models := []struct {
		id                   string
		host                 string
		official             bool
		configuredContext    uint64
		configuredCompletion uint64
		wantContext          string
		wantCompletionTokens string
	}{
		{
			id: "deepseek-v4-pro", host: "api.deepseek.com", official: true, configuredContext: 131072, configuredCompletion: 32768,
			wantContext: "131072", wantCompletionTokens: "32768",
		},
		{
			id: "deepseek-v4-flash", host: "api.deepseek.com", official: true,
			wantContext: "777777", wantCompletionTokens: "8192",
		},
		{
			id: "deepseek-future-model", host: "api.deepseek.com", official: true,
			wantContext: "777777", wantCompletionTokens: "8192",
		},
		{
			id: "gpt-future-model", host: "api.openai.com", configuredContext: 128000, configuredCompletion: 32000,
			wantContext: "128000", wantCompletionTokens: "32000",
		},
		{
			id: "claude-future-model", host: "api.anthropic.com", configuredContext: 200000,
			wantContext: "200000", wantCompletionTokens: "8192",
		},
		{
			id: "grok-future-model", host: "api.x.ai", configuredContext: 262144, configuredCompletion: 65536,
			wantContext: "262144", wantCompletionTokens: "65536",
		},
		{
			id: "gemini-future-model", host: "generativelanguage.googleapis.com", configuredContext: 1048576, configuredCompletion: 65536,
			wantContext: "1048576", wantCompletionTokens: "65536",
		},
		{id: "generic-configured", configuredContext: 262144, wantContext: "262144", wantCompletionTokens: "8192"},
		{id: "generic-remote", wantContext: "777777", wantCompletionTokens: "8192"},
	}
	backends := []string{"responses", "chat_completions", "messages"}

	for _, model := range models {
		for _, backend := range backends {
			t.Run(model.id+"/"+backend, func(t *testing.T) {
				var upstreamRequest []byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					upstreamRequest, _ = io.ReadAll(request.Body)
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set(grokContextWindowHeader, "777777")
					w.Header().Set(grokMaxCompletionTokensHeader, "8192")
					switch backend {
					case "responses":
						_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_usage","object":"response","created_at":1,"status":"completed","model":"`+model.id+`","output":[],"usage":{"input_tokens":80,"output_tokens":5,"total_tokens":123}}}`+"\n\n")
					case "chat_completions":
						_, _ = io.WriteString(w,
							`data: {"id":"chat_usage","object":"chat.completion.chunk","model":"`+model.id+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":null}`+"\n\n"+
								`data: {"id":"chat_usage","object":"chat.completion.chunk","model":"`+model.id+`","choices":[],"usage":{"prompt_tokens":80,"completion_tokens":5,"total_tokens":85}}`+"\n\n"+
								"data: [DONE]\n\n")
					case "messages":
						_, _ = io.WriteString(w,
							`data: {"type":"message_start","message":{"id":"msg_usage","type":"message","role":"assistant","content":[],"model":"`+model.id+`","usage":{"input_tokens":10,"cache_read_input_tokens":20,"cache_creation_input_tokens":30,"output_tokens":0}}}`+"\n\n"+
								`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`+"\n\n"+
								`data: {"type":"message_stop"}`+"\n\n")
					}
				}))
				defer upstream.Close()

				route := facadeRoute("usage-"+model.id+"-"+backend, backend, model.id, "key", upstream.URL)
				route.SupportsBackendSearch = false
				if model.host != "" {
					route.Host = model.host
				}
				if model.configuredContext != 0 {
					route.ContextWindow = model.configuredContext
					route.ContextWindowConfigured = true
				}
				if model.configuredCompletion != 0 {
					route.MaxCompletionTokens = model.configuredCompletion
					route.MaxCompletionTokensConfigured = true
				}
				s := New(log.New(io.Discard, "", 0))
				s.SetRoutes([]config.Route{route})
				startPathTestServer(t, s)

				protocol := backendProtocol(t, backend)
				requestBody := nativeRequestBody(backend, true)
				if backend == "chat_completions" {
					// Match Grok Build's StreamingChatRequest rather than the generic
					// integration-test fixture.
					requestBody = []byte(`{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`)
				}
				data, status, header := postFacadeProtocol(t, s, route.ChannelID, protocol, requestBody, "", nil)
				if status != http.StatusOK {
					t.Fatalf("status=%d body=%s", status, data)
				}
				requestRoot, err := decodeRequestObject(upstreamRequest)
				if err != nil || requestRoot["stream"] != true {
					t.Fatalf("upstream request=%s err=%v", upstreamRequest, err)
				}
				if backend == "chat_completions" {
					options, _ := requestRoot["stream_options"].(map[string]any)
					if options["include_usage"] != true {
						t.Fatalf("Chat usage was not requested: %s", upstreamRequest)
					}
				}

				if header.Get(grokContextWindowHeader) != model.wantContext || header.Get(grokMaxCompletionTokensHeader) != model.wantCompletionTokens {
					t.Fatalf("model metadata context=%q completion=%q", header.Get(grokContextWindowHeader), header.Get(grokMaxCompletionTokensHeader))
				}

				var payloads []map[string]any
				err = scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
					if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
						return nil
					}
					root, decodeErr := decodeJSONMap(payload)
					if decodeErr == nil {
						payloads = append(payloads, root)
					}
					return decodeErr
				})
				if err != nil {
					t.Fatal(err)
				}

				switch backend {
				case "responses":
					response, _ := payloads[len(payloads)-1]["response"].(map[string]any)
					usage, _ := response["usage"].(map[string]any)
					context, hasContext := usage["context_details"].(map[string]any)
					if numberInt(usage["total_tokens"]) != 123 || !hasContext {
						t.Fatalf("Responses usage=%#v", usage)
					}
					if numberInt(context["input_tokens"]) != 80 || numberInt(context["output_tokens"]) != 5 {
						t.Fatalf("live context=%#v", context)
					}
				case "chat_completions":
					usage, _ := payloads[len(payloads)-1]["usage"].(map[string]any)
					if numberInt(usage["total_tokens"]) != 85 {
						t.Fatalf("Chat usage=%#v", usage)
					}
				case "messages":
					message, _ := payloads[0]["message"].(map[string]any)
					startUsage, _ := message["usage"].(map[string]any)
					deltaUsage, _ := payloads[1]["usage"].(map[string]any)
					if numberInt(startUsage["input_tokens"])+numberInt(startUsage["cache_read_input_tokens"])+
						numberInt(startUsage["cache_creation_input_tokens"])+numberInt(deltaUsage["output_tokens"]) != 65 {
						t.Fatalf("Messages usage start=%#v delta=%#v", startUsage, deltaUsage)
					}
				}
			})
		}
	}
}

func TestTranslatedNonStreamingResponsesPreserveRemoteModelMetadata(t *testing.T) {
	for _, backend := range []string{"messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(grokContextWindowHeader, "777777")
				w.Header().Set(grokMaxCompletionTokensHeader, "8192")
				if backend == "messages" {
					_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
			}))
			defer upstream.Close()

			route := facadeRoute("metadata-"+backend, backend, "wire", "key", upstream.URL)
			route.SupportsBackendSearch = true
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if header.Get(grokContextWindowHeader) != "777777" || header.Get(grokMaxCompletionTokensHeader) != "8192" {
				t.Fatalf("remote metadata context=%q completion=%q", header.Get(grokContextWindowHeader), header.Get(grokMaxCompletionTokensHeader))
			}
		})
	}
}

func TestNormalizeNativeChatUsage(t *testing.T) {
	tests := []struct {
		name       string
		usage      any
		wantCached int
		wantPrompt int
		wantTotal  int
		wantDetail bool
		wantNull   bool
	}{
		{
			name: "DeepSeek fields are exposed to Grok Build",
			usage: map[string]any{
				"prompt_tokens":            30,
				"completion_tokens":        5,
				"total_tokens":             35,
				"prompt_cache_hit_tokens":  20,
				"prompt_cache_miss_tokens": 10,
			},
			wantCached: 20,
			wantPrompt: 30,
			wantTotal:  35,
			wantDetail: true,
		},
		{
			name: "standard field wins",
			usage: map[string]any{
				"prompt_tokens":           30,
				"completion_tokens":       5,
				"total_tokens":            35,
				"prompt_cache_hit_tokens": 20,
				"prompt_tokens_details":   map[string]any{"cached_tokens": 7, "future": true},
			},
			wantCached: 7,
			wantPrompt: 30,
			wantTotal:  35,
			wantDetail: true,
		},
		{
			name:       "invalid provider field is not guessed",
			usage:      map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35, "prompt_cache_hit_tokens": "20"},
			wantPrompt: 30,
			wantTotal:  35,
			wantDetail: false,
		},
		{
			name: "null standard container is populated from DeepSeek",
			usage: map[string]any{
				"prompt_tokens":           30,
				"completion_tokens":       5,
				"total_tokens":            35,
				"prompt_cache_hit_tokens": 20,
				"prompt_tokens_details":   nil,
			},
			wantCached: 20,
			wantPrompt: 30,
			wantTotal:  35,
			wantDetail: true,
		},
		{
			name: "total is derived from complete standard counts",
			usage: map[string]any{
				"prompt_tokens":     30,
				"completion_tokens": 5,
			},
			wantPrompt: 30,
			wantTotal:  35,
		},
		{
			name: "input and output aliases are projected",
			usage: map[string]any{
				"input_tokens":  30,
				"output_tokens": 5,
				"total_tokens":  99,
			},
			wantPrompt: 30,
			wantTotal:  35,
		},
		{
			name: "prompt and total are derived from complete DeepSeek cache counts",
			usage: map[string]any{
				"completion_tokens":        5,
				"prompt_cache_hit_tokens":  20,
				"prompt_cache_miss_tokens": 10,
			},
			wantCached: 20,
			wantPrompt: 30,
			wantTotal:  35,
			wantDetail: true,
		},
		{name: "missing required count", usage: map[string]any{"prompt_tokens": 30, "total_tokens": 35}, wantNull: true},
		{name: "partial DeepSeek cache counts", usage: map[string]any{"completion_tokens": 5, "prompt_cache_hit_tokens": 20}, wantNull: true},
		{name: "conflicting total is normalized for live context", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 99}, wantPrompt: 30, wantTotal: 35},
		{name: "conflicting DeepSeek cache partition", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35, "prompt_cache_hit_tokens": 20, "prompt_cache_miss_tokens": 9}, wantNull: true},
		{name: "non object usage", usage: "unavailable", wantNull: true},
		{name: "negative count", usage: map[string]any{"prompt_tokens": -1, "completion_tokens": 5, "total_tokens": 35}, wantNull: true},
		{name: "fractional count", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 1.5, "total_tokens": 35}, wantNull: true},
		{name: "overflowing count", usage: map[string]any{"prompt_tokens": uint64(maxCanonicalTokenCount) + 1, "completion_tokens": 5, "total_tokens": 35}, wantNull: true},
		{name: "wrong details container", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35, "prompt_tokens_details": "bad"}, wantNull: true},
		{name: "null detail count", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35, "completion_tokens_details": map[string]any{"reasoning_tokens": nil}}, wantNull: true},
		{name: "invalid cost", usage: map[string]any{"prompt_tokens": 30, "completion_tokens": 5, "total_tokens": 35, "cost_in_usd_ticks": "1"}, wantNull: true},
		{name: "all-zero placeholder", usage: map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}, wantNull: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := map[string]any{"usage": test.usage}
			normalizeNativeChatUsage(root)
			if test.wantNull {
				if root["usage"] != nil {
					t.Fatalf("usage=%#v want null", root["usage"])
				}
				return
			}
			usage, ok := root["usage"].(map[string]any)
			if !ok {
				t.Fatalf("usage=%#v want object", root["usage"])
			}
			details, ok := usage["prompt_tokens_details"].(map[string]any)
			if ok != test.wantDetail {
				t.Fatalf("details=%#v wantDetail=%t", usage["prompt_tokens_details"], test.wantDetail)
			}
			cached, present, valid := optionalCanonicalToken(details, "cached_tokens")
			if ok && (!present || !valid || cached != int64(test.wantCached)) {
				t.Fatalf("cached_tokens=%v want=%d", details["cached_tokens"], test.wantCached)
			}
			prompt, promptPresent, promptValid := optionalCanonicalToken(usage, "prompt_tokens")
			total, totalPresent, totalValid := optionalCanonicalToken(usage, "total_tokens")
			if !promptPresent || !promptValid || !totalPresent || !totalValid ||
				prompt != int64(test.wantPrompt) || total != int64(test.wantTotal) {
				t.Fatalf("normalized usage=%#v want prompt=%d total=%d", usage, test.wantPrompt, test.wantTotal)
			}
		})
	}
}

func TestNativeChatCacheUsageProjectionWorksForJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w,
						`data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+
							`data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35,"prompt_cache_hit_tokens":20,"prompt_cache_miss_tokens":10}}`+"\n\n"+
							"data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"completion_tokens":5,"total_tokens":35,"prompt_cache_hit_tokens":20,"prompt_cache_miss_tokens":10}}`)
			}))
			defer upstream.Close()

			route := facadeRoute("native-chat-cache", "chat_completions", "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			requestBody := []byte(`{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"stream_options":{"include_usage":true}}`)
			data, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireChatCompletions, requestBody, "", nil)
			if status != http.StatusOK || !bytes.Contains(data, []byte(`"prompt_tokens_details":{"cached_tokens":20}`)) {
				t.Fatalf("status=%d body=%s", status, data)
			}
		})
	}
}

func TestNativeChatDerivesProvableUsageAcrossProvidersAndTransports(t *testing.T) {
	providers := []struct {
		name       string
		host       string
		usage      string
		wantCached int
	}{
		{
			name:  "generic standard counts",
			host:  "relay.example",
			usage: `{"prompt_tokens":30,"completion_tokens":5}`,
		},
		{
			name:  "generic conflicting total",
			host:  "relay.example",
			usage: `{"prompt_tokens":30,"completion_tokens":5,"total_tokens":99}`,
		},
		{
			name:  "future input output aliases",
			host:  "relay.example",
			usage: `{"input_tokens":30,"output_tokens":5,"total_tokens":99}`,
		},
		{
			name:       "DeepSeek cache partition",
			host:       "api.deepseek.com",
			usage:      `{"completion_tokens":5,"prompt_cache_hit_tokens":20,"prompt_cache_miss_tokens":10}`,
			wantCached: 20,
		},
	}
	for _, provider := range providers {
		for _, stream := range []bool{false, true} {
			t.Run(provider.name+"/"+map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w,
							`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"future-model","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+
								`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"future-model","choices":[],"usage":`+provider.usage+`}`+"\n\n"+
								"data: [DONE]\n\n")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","created":1,"model":"future-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":`+provider.usage+`}`)
				}))
				defer upstream.Close()

				route := facadeRoute("native-chat-derived-usage", "chat_completions", "future-model", "key", upstream.URL)
				route.Host = provider.host
				s := New(log.New(io.Discard, "", 0))
				s.SetRoutes([]config.Route{route})
				startPathTestServer(t, s)

				requestBody := []byte(`{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"stream_options":{"include_usage":true}}`)
				data, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireChatCompletions, requestBody, "", nil)
				if status != http.StatusOK {
					t.Fatalf("status=%d body=%s", status, data)
				}

				var usage map[string]any
				if stream {
					if err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
						if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
							return nil
						}
						root, err := decodeJSONMap(payload)
						if err == nil {
							usage, _ = root["usage"].(map[string]any)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				} else {
					root, err := decodeJSONMap(data)
					if err != nil {
						t.Fatal(err)
					}
					usage, _ = root["usage"].(map[string]any)
				}
				if numberInt(usage["prompt_tokens"]) != 30 || numberInt(usage["completion_tokens"]) != 5 ||
					numberInt(usage["total_tokens"]) != 35 {
					t.Fatalf("derived usage is incomplete: %#v", usage)
				}
				details, _ := usage["prompt_tokens_details"].(map[string]any)
				if numberInt(details["cached_tokens"]) != provider.wantCached {
					t.Fatalf("cached usage=%#v want=%d", details, provider.wantCached)
				}
			})
		}
	}
}

func TestNativeChatInvalidUsageBecomesNullForJSONAndSSE(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w,
						`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n"+
							`data: {"id":"chat_1","object":"chat.completion.chunk","created":1,"model":"wire","choices":[],"usage":{"prompt_tokens":30,"total_tokens":35}}`+"\n\n"+
							"data: [DONE]\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","created":1,"model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":30,"total_tokens":35}}`)
			}))
			defer upstream.Close()

			route := facadeRoute("native-chat-invalid-usage", "chat_completions", "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			requestBody := []byte(`{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `,"stream_options":{"include_usage":true}}`)
			data, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireChatCompletions, requestBody, "", nil)
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if !stream {
				root, err := decodeJSONMap(data)
				if err != nil {
					t.Fatal(err)
				}
				if usage, present := root["usage"]; !present || usage != nil {
					t.Fatalf("invalid JSON usage was not normalized to null: %s", data)
				}
				return
			}

			found := false
			if err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
				if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
					return nil
				}
				root, err := decodeJSONMap(payload)
				if err != nil {
					return err
				}
				if usage, present := root["usage"]; present {
					found = true
					if usage != nil {
						t.Fatalf("invalid SSE usage was not normalized to null: %#v", root)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatalf("normalized SSE usage frame missing: %s", data)
			}
		})
	}
}

func TestNativeMessagesUsageAndEventsMatchGrokBuildTypes(t *testing.T) {
	valid, err := decodeJSONMap([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":"future_reason","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativeMessagesEnvelope(valid); err != nil {
		t.Fatalf("valid Messages response was rejected: %v", err)
	}

	invalidUsage := []string{
		`{"output_tokens":1}`,
		`{"input_tokens":1}`,
		`{"input_tokens":-1,"output_tokens":1}`,
		`{"input_tokens":1.5,"output_tokens":1}`,
		`{"input_tokens":4294967296,"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":-1}`,
	}
	for _, rawUsage := range invalidUsage {
		root, err := decodeJSONMap([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":` + rawUsage + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateNativeMessagesEnvelope(root); err == nil {
			t.Fatalf("invalid Messages usage was accepted: %s", rawUsage)
		}
	}

	validEvents := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":1,"output_tokens":0}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"future_reason","stop_details":{"type":"future","explanation":"done"}},"usage":{"output_tokens":1}}`,
		`{"type":"message_stop"}`,
		`{"type":"ping"}`,
		`{"type":"error","error":{"type":"upstream_error","message":"failed"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"future_content","payload":true}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"future_delta","value":"x"}}`,
		`{"type":"future_event","payload":true}`,
	}
	for _, raw := range validEvents {
		event, err := decodeJSONMap([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateNativeSSEFrame(wireMessages, event); err != nil {
			t.Fatalf("valid Messages event was rejected: %s: %v", raw, err)
		}
	}

	invalidEvents := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"output_tokens":0}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{}}`,
		`{"type":"message_delta","delta":{"stop_reason":7},"usage":{"output_tokens":1}}`,
		`{"type":"content_block_start","content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1.5,"delta":{"type":"text_delta","text":"x"}}`,
		`{"type":"content_block_stop","index":4294967296}`,
		`{"type":"error","error":{"type":"upstream_error"}}`,
	}
	for _, raw := range invalidEvents {
		event, err := decodeJSONMap([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateNativeSSEFrame(wireMessages, event); err == nil {
			t.Fatalf("Messages event Grok Build cannot decode was accepted: %s", raw)
		}
	}
}

func TestNativeChatNormalizationAndValidationMatchGrokBuildTypes(t *testing.T) {
	route := config.Route{WireModel: "future-provider-model"}
	response, err := decodeJSONMap([]byte(`{"choices":[{"message":{"content":"ok","tool_calls":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0,"cost_in_usd_ticks":0}}`))
	if err != nil {
		t.Fatal(err)
	}
	normalizeNativeChatRequiredFields(response, route, false, "chatcmpl_fallback", 123)
	normalizeNativeChatUsage(response)
	if err := validateNativeChatEnvelope(response); err != nil {
		t.Fatalf("normalized Chat response was rejected: %v; %#v", err, response)
	}
	choice := anySlice(response["choices"])[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if response["id"] != "chatcmpl_fallback" || response["object"] != "chat.completion" ||
		response["created"] != int64(123) || response["model"] != "future-provider-model" ||
		numberInt(choice["index"]) != 0 || message["role"] != "assistant" || len(anySlice(message["tool_calls"])) != 0 {
		t.Fatalf("Chat response required fields were not normalized: id=%#v object=%#v created=%#v model=%#v index=%#v role=%#v tool_calls=%#v full=%#v",
			response["id"], response["object"], response["created"], response["model"], choice["index"], message["role"], message["tool_calls"], response)
	}

	chunk, err := decodeJSONMap([]byte(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":null}`))
	if err != nil {
		t.Fatal(err)
	}
	normalizeNativeChatRequiredFields(chunk, route, true, "chatcmpl_stream", 456)
	normalizeNativeChatUsage(chunk)
	if err := validateNativeChatChunk(chunk); err != nil {
		t.Fatalf("normalized Chat chunk was rejected: %v; %#v", err, chunk)
	}
	if chunk["id"] != "chatcmpl_stream" || chunk["object"] != "chat.completion.chunk" ||
		chunk["created"] != int64(456) || chunk["model"] != "future-provider-model" {
		t.Fatalf("Chat chunk required fields were not normalized: %#v", chunk)
	}
	futureChunk, err := decodeJSONMap([]byte(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"future_role"},"finish_reason":"future_reason"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativeChatChunk(futureChunk); err != nil {
		t.Fatalf("future native Chat enum values were rejected: %v", err)
	}

	invalidChunks := []string{
		`{"id":"c","object":"chat.completion.chunk","created":-1,"model":"m","choices":[]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":4294967296,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"finish_reason":"stop"}]}`,
		`{"id":"c","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"   "},"finish_reason":null}]}`,
	}
	for _, raw := range invalidChunks {
		root, err := decodeJSONMap([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if err := validateNativeChatChunk(root); err == nil {
			t.Fatalf("Chat chunk Grok Build cannot decode was accepted: %s", raw)
		}
	}
}

func TestNativeChatCostRejectsInvalidMeasurements(t *testing.T) {
	valid := []any{json.Number("0"), json.Number("1"), int(0), int64(1), uint64(^uint64(0) >> 1)}
	for _, value := range valid {
		if !validOptionalChatCost(map[string]any{"cost_in_usd_ticks": value}) {
			t.Fatalf("valid cost was rejected: %#v", value)
		}
	}
	invalid := []any{json.Number("-1"), json.Number("1.5"), json.Number("9223372036854775808"), int(-1), int64(-1), uint64(^uint64(0)>>1) + 1}
	for _, value := range invalid {
		usage := map[string]any{
			"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
			"cost_in_usd_ticks": value,
		}
		if validOptionalChatCost(usage) {
			t.Fatalf("invalid cost was accepted: %#v", value)
		}
		root := map[string]any{"usage": usage}
		normalizeNativeChatUsage(root)
		if root["usage"] != nil {
			t.Fatalf("invalid cost measurement reached Grok Build: %#v", root)
		}
	}
}

func TestNativeMessagesAllowsFutureExtensionsButRejectsMalformedKnownShapes(t *testing.T) {
	futureBlock := map[string]any{"type": "future_content", "payload": map[string]any{"value": 1}}
	futureDelta := map[string]any{"type": "future_delta", "value": "x"}
	message := map[string]any{
		"id": "msg_future", "type": "message", "role": "assistant", "model": "future-model",
		"content": []any{futureBlock}, "stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 3, "output_tokens": 2},
	}
	if err := validateNativeMessagesEnvelope(message); err != nil {
		t.Fatalf("native validator rejected a future content block: %v", err)
	}
	if err := validateMessagesEnvelope(message); err == nil {
		t.Fatal("strict cross-protocol validator accepted a future content block")
	}
	if err := validateNativeMessagesStreamDelta(futureDelta); err != nil {
		t.Fatalf("native validator rejected a future delta: %v", err)
	}
	if err := validateMessagesStreamDelta(futureDelta); err == nil {
		t.Fatal("strict cross-protocol validator accepted a future delta")
	}
	if err := validateNativeSSEFrame(wireMessages, map[string]any{"type": "future_event", "payload": true}); err != nil {
		t.Fatalf("native validator rejected a future event: %v", err)
	}
	for _, invalid := range []map[string]any{
		{"type": ""},
		{"type": "content_block_start", "content_block": map[string]any{"type": "text"}},
		{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta"}},
	} {
		if err := validateNativeSSEFrame(wireMessages, invalid); err == nil {
			t.Fatalf("malformed known native event was accepted: %#v", invalid)
		}
	}
}

func TestNativeMessagesUnknownWireVariantsPassThrough(t *testing.T) {
	tests := []struct {
		name                string
		upstreamContentType string
		upstreamBody        string
	}{
		{
			name:                "json buffered fallback",
			upstreamContentType: "application/json",
			upstreamBody:        `{"id":"msg_future","type":"message","role":"assistant","model":"future-model","content":[{"type":"future_content","payload":{"value":1}}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`,
		},
		{
			name:                "native sse",
			upstreamContentType: "text/event-stream",
			upstreamBody: `data: {"type":"message_start","message":{"id":"msg_future","type":"message","role":"assistant","model":"future-model","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"future_content","payload":{"value":1}}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"future_delta","value":"x"}}` + "\n\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				`data: {"type":"future_event","payload":true}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", test.upstreamContentType)
				_, _ = io.WriteString(w, test.upstreamBody)
			}))
			defer upstream.Close()

			route := facadeRoute("native-messages-future", "messages", "future-model", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			requestBody := []byte(`{"model":"display","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":true}`)
			data, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireMessages, requestBody, "", nil)
			if status != http.StatusOK || bytes.Contains(data, []byte(`"type":"proxy_stream_error"`)) ||
				!bytes.Contains(data, []byte(`"type":"future_content"`)) {
				t.Fatalf("future native extension was not passed through: status=%d body=%s", status, data)
			}
			if test.upstreamContentType == "text/event-stream" &&
				(!bytes.Contains(data, []byte(`"type":"future_delta"`)) || !bytes.Contains(data, []byte(`"type":"future_event"`))) {
				t.Fatalf("future native SSE extension was not passed through: status=%d body=%s", status, data)
			}
		})
	}
}
