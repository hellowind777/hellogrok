package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestOfficialDeepSeekRecognitionIsHostBased(t *testing.T) {
	tests := []struct {
		model   string
		backend string
	}{
		{"deepseek-v4-pro", "responses"},
		{" DeepSeek-V4-Flash ", "chat_completions"},
		{"deepseek-v4-pro[1m]", "messages"},
		{"deepseek-chat", "chat_completions"},
		{"deepseek-future-model", "responses"},
	}
	for _, test := range tests {
		t.Run(test.model+"/"+test.backend, func(t *testing.T) {
			route := config.Route{Host: "api.deepseek.com", WireModel: test.model, APIBackend: test.backend}
			if !isOfficialDeepSeekRoute(route) {
				t.Fatalf("official DeepSeek route was not recognized for future-compatible model %q", test.model)
			}
		})
	}

	if isOfficialDeepSeekRoute(config.Route{Host: "api.deepseek.com.evil", WireModel: "deepseek-v4-pro"}) {
		t.Fatal("lookalike DeepSeek host was treated as official")
	}
	if isOfficialDeepSeekRoute(config.Route{Host: "relay.example", WireModel: "deepseek-future-model"}) {
		t.Fatal("relay reusing a DeepSeek model name was treated as official")
	}
}

func TestDeepSeekAuthenticationMatchesEachNativeProtocol(t *testing.T) {
	tests := []struct {
		name       string
		protocol   wireProtocol
		wantBearer string
		wantAPIKey string
	}{
		{name: "responses", protocol: wireResponses, wantBearer: "Bearer channel-key"},
		{name: "chat", protocol: wireChatCompletions, wantBearer: "Bearer channel-key"},
		{name: "messages", protocol: wireMessages, wantAPIKey: "channel-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{
				"Authorization": []string{"Bearer login-token"},
				"X-Api-Key":     []string{"login-api-key"},
			}
			applyRouteHeaders(header, config.Route{
				Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", APIKey: "channel-key",
			}, test.protocol, nil)
			if header.Get("Authorization") != test.wantBearer || header.Get("X-Api-Key") != test.wantAPIKey {
				t.Fatalf("authorization=%q x-api-key=%q", header.Get("Authorization"), header.Get("X-Api-Key"))
			}
		})
	}
}

func TestDeepSeekMessagesAuthenticationDropsConflictingBearer(t *testing.T) {
	header := http.Header{}
	applyRouteHeaders(header, config.Route{
		Host:       "api.deepseek.com",
		WireModel:  "deepseek-v4-pro",
		APIBackend: "messages",
		APIKey:     "channel-key",
		ExtraHeaders: map[string]string{
			"Authorization": "Bearer stale-key",
		},
	}, wireMessages, nil)

	if got := header.Get("X-Api-Key"); got != "channel-key" {
		t.Fatalf("x-api-key=%q want channel-key", got)
	}
	if got := header.Get("Authorization"); got != "" {
		t.Fatalf("conflicting authorization header was retained: %q", got)
	}
}

func TestDeepSeekDynamicAuthenticationConvertsAcrossProtocolBridges(t *testing.T) {
	tests := []struct {
		name       string
		route      config.Route
		protocol   wireProtocol
		incoming   http.Header
		wantBearer string
		wantAPIKey string
	}{
		{
			name: "bearer facade to messages",
			route: config.Route{
				Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", APIBackend: "messages",
				DynamicAuth: true, IncomingAuthScheme: "bearer",
			},
			protocol:   wireMessages,
			incoming:   http.Header{"Authorization": []string{"Bearer dynamic-key"}},
			wantAPIKey: "dynamic-key",
		},
		{
			name: "x-api-key facade to responses",
			route: config.Route{
				Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", APIBackend: "messages",
				DynamicAuth: true, IncomingAuthScheme: "x_api_key",
			},
			protocol:   wireResponses,
			incoming:   http.Header{"X-Api-Key": []string{"dynamic-key"}},
			wantBearer: "Bearer dynamic-key",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			applyRouteHeaders(header, test.route, test.protocol, test.incoming)
			if header.Get("Authorization") != test.wantBearer || header.Get("X-Api-Key") != test.wantAPIKey {
				t.Fatalf("authorization=%q x-api-key=%q", header.Get("Authorization"), header.Get("X-Api-Key"))
			}
		})
	}
}

func TestNormalizeDeepSeekRequestKeepsModelIDsDataDrivenAndHandlesMessagesAlias(t *testing.T) {
	tests := []struct {
		name     string
		route    config.Route
		protocol wireProtocol
		want     string
	}{
		{
			name:     "responses trims configured model",
			route:    config.Route{Host: "api.deepseek.com", WireModel: " DeepSeek-V4-Flash ", APIBackend: "responses"},
			protocol: wireResponses,
			want:     "DeepSeek-V4-Flash",
		},
		{
			name:     "chat trims configured model",
			route:    config.Route{Host: "api.deepseek.com", WireModel: " DeepSeek-V4-Pro ", APIBackend: "chat_completions"},
			protocol: wireChatCompletions,
			want:     "DeepSeek-V4-Pro",
		},
		{
			name:     "messages preserves canonical one million alias",
			route:    config.Route{Host: "api.deepseek.com", WireModel: " DeepSeek-V4-Pro[1M] ", APIBackend: "messages"},
			protocol: wireMessages,
			want:     "DeepSeek-V4-Pro[1m]",
		},
		{
			name:     "responses strips messages-only alias during a bridge",
			route:    config.Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro[1m]", APIBackend: "messages"},
			protocol: wireResponses,
			want:     "deepseek-v4-pro",
		},
		{
			name:     "future model passes through unchanged",
			route:    config.Route{Host: "api.deepseek.com", WireModel: "deepseek-future-model", APIBackend: "responses"},
			protocol: wireResponses,
			want:     "deepseek-future-model",
		},
		{
			name:     "future messages alias is handled generically",
			route:    config.Route{Host: "api.deepseek.com", WireModel: "deepseek-future-model[1M]", APIBackend: "messages"},
			protocol: wireResponses,
			want:     "deepseek-future-model",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := map[string]any{"model": "display-model"}
			normalizeDeepSeekRequest(root, test.route, test.protocol)
			if root["model"] != test.want {
				t.Fatalf("model=%q want %q", root["model"], test.want)
			}
		})
	}
}

func TestCanonicalDeepSeekResponseUsesProviderModelID(t *testing.T) {
	route := config.Route{
		Host:       "api.deepseek.com",
		WireModel:  " DeepSeek-V4-Pro[1M] ",
		APIBackend: "messages",
	}
	response := canonicalResponse(route, facadeRequest{}, canonicalResult{})
	if response["model"] != "DeepSeek-V4-Pro" {
		t.Fatalf("response model=%q want configured provider model ID", response["model"])
	}

	relay := config.Route{Host: "relay.example", WireModel: "provider-model[1m]"}
	response = canonicalResponse(relay, facadeRequest{}, canonicalResult{})
	if response["model"] != "provider-model[1m]" {
		t.Fatalf("non-DeepSeek model alias was rewritten: %#v", response)
	}
}

func TestNormalizeDeepSeekResponsesPreservesDocumentedToolShapes(t *testing.T) {
	root := map[string]any{
		"model":     "display",
		"reasoning": map[string]any{"effort": "xhigh", "summary": "auto"},
		"tools": []any{
			map[string]any{"type": "custom", "name": "apply_patch"},
			map[string]any{"type": "web_search_2025_08_26"},
		},
		"tool_choice": map[string]any{"type": "web_search_2025_08_26"},
	}
	normalizeDeepSeekRequest(root, config.Route{
		Host: "api.deepseek.com", WireModel: "deepseek-future-model",
	}, wireResponses)
	if root["model"] != "deepseek-future-model" {
		t.Fatalf("future model ID was not preserved: %#v", root)
	}
	reasoning := root["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" || reasoning["summary"] != "auto" {
		t.Fatalf("Responses reasoning normalization=%#v", reasoning)
	}
	tools := anySlice(root["tools"])
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "apply_patch" ||
		tools[1].(map[string]any)["type"] != "web_search_2025_08_26" {
		t.Fatalf("DeepSeek native tools changed: %#v", root)
	}
	if root["tool_choice"].(map[string]any)["type"] != "web_search_2025_08_26" {
		t.Fatalf("DeepSeek native tool choice changed: %#v", root)
	}
}

func TestNormalizeDeepSeekMessagesThinking(t *testing.T) {
	t.Run("low effort stays low", func(t *testing.T) {
		root := map[string]any{
			"thinking":      map[string]any{"type": "adaptive", "budget_tokens": 1000},
			"output_config": map[string]any{"effort": "low"},
		}
		normalizeDeepSeekMessagesRequest(root, true)
		thinking := root["thinking"].(map[string]any)
		output := root["output_config"].(map[string]any)
		if thinking["type"] != "enabled" || output["effort"] != "low" || thinking["budget_tokens"] != 1000 {
			t.Fatalf("Messages thinking normalization=%#v", root)
		}
	})

	t.Run("none disables thinking and removes unsupported format", func(t *testing.T) {
		root := map[string]any{
			"thinking": map[string]any{"type": "adaptive"},
			"output_config": map[string]any{
				"effort": "none",
				"format": map[string]any{"type": "json_schema"},
			},
		}
		normalizeDeepSeekMessagesRequest(root, true)
		if root["thinking"].(map[string]any)["type"] != "disabled" {
			t.Fatalf("none did not disable thinking: %#v", root)
		}
		if _, exists := root["output_config"]; exists {
			t.Fatalf("unsupported Messages output_config survived disabled thinking: %#v", root)
		}
	})

	t.Run("enabled effort is the only supported output config field", func(t *testing.T) {
		root := map[string]any{
			"thinking": map[string]any{"type": "adaptive"},
			"output_config": map[string]any{
				"effort": "xhigh",
				"format": map[string]any{"type": "json_schema"},
				"opaque": true,
			},
		}
		normalizeDeepSeekMessagesRequest(root, true)
		output := root["output_config"].(map[string]any)
		if len(output) != 1 || output["effort"] != "xhigh" {
			t.Fatalf("unsupported Messages output config was forwarded: %#v", root)
		}
	})

	t.Run("omitted Grok none disables provider default", func(t *testing.T) {
		root := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
		normalizeDeepSeekMessagesRequest(root, true)
		thinking := root["thinking"].(map[string]any)
		if thinking["type"] != "disabled" {
			t.Fatalf("omitted Messages None did not disable DeepSeek thinking: %#v", root)
		}
	})

	t.Run("future model without a configured menu preserves provider default", func(t *testing.T) {
		root := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
		normalizeDeepSeekMessagesRequest(root, false)
		if _, exists := root["thinking"]; exists {
			t.Fatalf("future model thinking default was overridden: %#v", root)
		}
	})

	t.Run("future model keeps explicitly configured effort value", func(t *testing.T) {
		root := map[string]any{
			"thinking":      map[string]any{"type": "enabled"},
			"output_config": map[string]any{"effort": "future-tier", "format": "ignored"},
		}
		normalizeDeepSeekMessagesRequest(root, true)
		output := root["output_config"].(map[string]any)
		if len(output) != 1 || output["effort"] != "future-tier" {
			t.Fatalf("provider-owned effort was changed: %#v", root)
		}
	})

	t.Run("malformed provider fields are left for upstream validation", func(t *testing.T) {
		for _, field := range []string{"thinking", "output_config"} {
			root := map[string]any{field: "invalid"}
			normalizeDeepSeekMessagesRequest(root, true)
			if len(root) != 1 || root[field] != "invalid" {
				t.Fatalf("malformed %s was rewritten: %#v", field, root)
			}
		}
	})
}

func TestDeepSeekMessagesReasoningSelectionFromConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	raw := `[model.explicit-none]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com"
api_key = "test-key"
api_backend = "messages"
reasoning_effort = "none"

[model.provider-default]
model = "deepseek-v4-pro"
base_url = "https://api.deepseek.com"
api_key = "test-key"
api_backend = "messages"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	models, err := config.LoadModels(path)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := config.BuildRoutes(models)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]config.Route, len(routes))
	for _, route := range routes {
		byID[route.ChannelID] = route
	}

	explicit := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	normalizeDeepSeekRequest(explicit, byID["explicit-none"], wireMessages)
	if thinking, _ := explicit["thinking"].(map[string]any); thinking["type"] != "disabled" {
		t.Fatalf("single reasoning_effort=none did not disable DeepSeek thinking: %#v", explicit)
	}

	unconfigured := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	normalizeDeepSeekRequest(unconfigured, byID["provider-default"], wireMessages)
	if _, exists := unconfigured["thinking"]; exists {
		t.Fatalf("unconfigured reasoning fields overrode the provider default: %#v", unconfigured)
	}
}

func TestFutureDeepSeekChatKeepsProviderOwnedReasoningEffort(t *testing.T) {
	root := map[string]any{
		"stream":           true,
		"user":             "tenant_1",
		"reasoning_effort": "future-tier",
		"thinking":         map[string]any{"type": "adaptive", "future": true},
		"tool_choice":      "auto",
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "future",
				"schema": map[string]any{"type": "object"},
			},
		},
		"messages": []any{
			map[string]any{"role": "developer", "content": "rules"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "call_1"}}},
		},
	}
	normalizeDeepSeekChatRequest(root)
	if root["reasoning_effort"] != "future-tier" || root["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("provider-owned Chat effort was changed: %#v", root)
	}
	if root["thinking"].(map[string]any)["future"] != true ||
		root["response_format"].(map[string]any)["type"] != "json_object" {
		t.Fatalf("future Chat protocol compatibility was not applied: %#v", root)
	}
	messages := anySlice(root["messages"])
	if len(messages) != 3 ||
		messages[0].(map[string]any)["role"] != "system" ||
		messages[1].(map[string]any)["role"] != "system" ||
		messages[2].(map[string]any)["content"] != "" {
		t.Fatalf("future Chat history missed protocol normalization: %#v", messages)
	}
	if _, hasUser := root["user"]; hasUser || root["user_id"] != "tenant_1" ||
		root["stream_options"].(map[string]any)["include_usage"] != true {
		t.Fatalf("host-level DeepSeek compatibility was lost: %#v", root)
	}
}

func TestNormalizeDeepSeekChatAgentCompatibility(t *testing.T) {
	root := map[string]any{
		"stream":           true,
		"stream_options":   map[string]any{"opaque": true},
		"reasoning_effort": "xhigh",
		"tool_choice":      "auto",
		"tools":            []any{map[string]any{"type": "function"}},
		"messages": []any{
			map[string]any{"role": "developer", "content": "rules"},
			map[string]any{
				"role": "assistant", "content": nil, "reasoning_content": "keep me",
				"tool_calls": []any{map[string]any{"id": "call_1"}},
			},
		},
	}
	normalizeDeepSeekChatRequest(root)
	if root["reasoning_effort"] != "xhigh" || root["thinking"].(map[string]any)["type"] != "enabled" {
		t.Fatalf("Chat effort normalization=%#v", root)
	}
	if _, exists := root["tool_choice"]; exists || len(anySlice(root["tools"])) != 1 {
		t.Fatalf("thinking-mode tool choice was not normalized=%#v", root)
	}
	options := root["stream_options"].(map[string]any)
	if options["include_usage"] != true || options["opaque"] != true {
		t.Fatalf("stream options were not merged: %#v", options)
	}
	messages := anySlice(root["messages"])
	if messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("developer role was not converted: %#v", messages[0])
	}
	assistant := messages[1].(map[string]any)
	if assistant["content"] != "" || assistant["reasoning_content"] != "keep me" {
		t.Fatalf("assistant tool history was not preserved: %#v", assistant)
	}
}

func TestNormalizeDeepSeekChatUsesDocumentedMaxTokens(t *testing.T) {
	root := map[string]any{
		"max_completion_tokens": 4096,
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1"}}, "future": true},
		},
	}
	normalizeDeepSeekChatRequest(root)
	if root["max_tokens"] != 4096 {
		t.Fatalf("max_completion_tokens was not mapped: %#v", root)
	}
	if _, exists := root["max_completion_tokens"]; exists {
		t.Fatalf("unsupported max_completion_tokens remained: %#v", root)
	}
	assistant := anySlice(root["messages"])[0].(map[string]any)
	if assistant["content"] != "" || assistant["future"] != true {
		t.Fatalf("tool-call history normalization lost fields: %#v", assistant)
	}

	first, err := encodeRequestObject(root)
	if err != nil {
		t.Fatal(err)
	}
	normalizeDeepSeekChatRequest(root)
	second, err := encodeRequestObject(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("DeepSeek Chat normalization is not idempotent:\nfirst=%s\nsecond=%s", first, second)
	}

	explicit := map[string]any{"max_tokens": 2048, "max_completion_tokens": 8192}
	normalizeDeepSeekChatRequest(explicit)
	if explicit["max_tokens"] != 2048 {
		t.Fatalf("explicit max_tokens did not win: %#v", explicit)
	}
	if _, exists := explicit["max_completion_tokens"]; exists {
		t.Fatalf("unsupported max_completion_tokens remained: %#v", explicit)
	}
}

func TestNormalizeDeepSeekChatMapsDocumentedUserID(t *testing.T) {
	root := map[string]any{"user": "tenant_user-42"}
	normalizeDeepSeekChatRequest(root)
	if root["user_id"] != "tenant_user-42" {
		t.Fatalf("Chat user_id mapping failed: %#v", root)
	}
	if _, exists := root["user"]; exists {
		t.Fatalf("unsupported Chat user field remained: %#v", root)
	}

	root = map[string]any{"user": "fallback", "user_id": "provider_owned"}
	normalizeDeepSeekChatRequest(root)
	if root["user_id"] != "provider_owned" {
		t.Fatalf("explicit DeepSeek user_id was overwritten: %#v", root)
	}
}

func TestNormalizeDeepSeekChatDisabledThinkingKeepsToolChoice(t *testing.T) {
	root := map[string]any{
		"reasoning_effort": "none",
		"tool_choice":      "required",
		"tools":            []any{map[string]any{"type": "function"}},
	}
	normalizeDeepSeekChatRequest(root)
	if root["thinking"].(map[string]any)["type"] != "disabled" || root["tool_choice"] != "required" ||
		len(anySlice(root["tools"])) != 1 {
		t.Fatalf("non-thinking tool semantics changed: %#v", root)
	}
	if _, exists := root["reasoning_effort"]; exists {
		t.Fatalf("none reasoning_effort was left on Chat request: %#v", root)
	}
}

func TestNormalizeDeepSeekChatDropsUnsupportedThinkingToolChoice(t *testing.T) {
	for _, choice := range []any{
		"required",
		map[string]any{"type": "function", "function": map[string]any{"name": "build"}},
	} {
		root := map[string]any{
			"reasoning_effort": "max",
			"tool_choice":      choice,
			"tools":            []any{map[string]any{"type": "function"}},
		}
		normalizeDeepSeekChatRequest(root)
		_, hasToolChoice := root["tool_choice"]
		if root["thinking"].(map[string]any)["type"] != "enabled" || root["reasoning_effort"] != "max" || hasToolChoice ||
			len(anySlice(root["tools"])) != 1 {
			t.Fatalf("thinking tool choice semantics changed: %#v", root)
		}
	}
}

func TestNormalizeDeepSeekChatThinkingToolChoiceNoneRemovesTools(t *testing.T) {
	root := map[string]any{
		"tool_choice": "none",
		"tools":       []any{map[string]any{"type": "function"}},
	}
	normalizeDeepSeekChatRequest(root)
	if _, hasChoice := root["tool_choice"]; hasChoice || len(anySlice(root["tools"])) != 0 {
		t.Fatalf("thinking-mode tool disable was not preserved: %#v", root)
	}
}

func TestNormalizeDeepSeekChatStructuredOutputUsesDocumentedJSONMode(t *testing.T) {
	root := map[string]any{
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "result",
				"strict": true,
				"schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"answer": map[string]any{"type": "string"}},
					"required":   []any{"answer"},
				},
			},
		},
		"messages": []any{map[string]any{"role": "user", "content": "answer"}},
	}
	normalizeDeepSeekChatRequest(root)

	format := root["response_format"].(map[string]any)
	if format["type"] != "json_object" || len(format) != 1 {
		t.Fatalf("Chat json_schema was not converted to documented JSON mode: %#v", format)
	}
	messages := anySlice(root["messages"])
	if len(messages) != 2 {
		t.Fatalf("structured-output instruction missing: %#v", messages)
	}
	system := messages[0].(map[string]any)
	content := stringValue(system["content"])
	if system["role"] != "system" || !strings.Contains(content, "JSON Schema") ||
		!strings.Contains(content, `"answer"`) {
		t.Fatalf("schema instruction was not preserved: %#v", system)
	}

	// Normalization is idempotent when a retry reuses an already adapted body.
	normalizeDeepSeekChatRequest(root)
	if len(anySlice(root["messages"])) != 2 {
		t.Fatalf("JSON instruction was duplicated: %#v", root["messages"])
	}
}

func TestDeepSeekResponsesRequestsSourcesWithoutReplacingCallerIncludes(t *testing.T) {
	route := config.Route{
		ChannelID: "deepseek", Host: "api.deepseek.com", WireModel: "deepseek-v4-pro",
		APIBackend: "responses", SupportsBackendSearch: true,
		ChatSearchDialect: config.ChatSearchDialectResponses,
	}
	request, err := adaptFacadeRequest([]byte(`{
		"model":"display",
		"input":"search",
		"tools":[{"type":"web_search"}],
		"include":["caller.owned"]
	}`), route, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeRequestObject(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	includes := anySlice(root["include"])
	if len(includes) != 2 || includes[0] != "caller.owned" || includes[1] != responsesWebSearchSourcesInclude {
		t.Fatalf("DeepSeek source hint or caller include changed: %#v", includes)
	}
}

func TestDeepSeekResponsesPreservesIncompleteAndFailedTerminalEvents(t *testing.T) {
	tests := []struct {
		name          string
		eventType     string
		status        string
		extra         string
		wantDetail    string
		wantErrorCode string
		wantErrorText string
	}{
		{
			name:       "incomplete",
			eventType:  "response.incomplete",
			status:     "incomplete",
			extra:      `,"incomplete_details":{"reason":"max_output_tokens"},"error":null`,
			wantDetail: "max_output_tokens",
		},
		{
			name:          "failed",
			eventType:     "response.failed",
			status:        "failed",
			extra:         `,"incomplete_details":null,"error":{"code":"server_error","message":"inference failed"}`,
			wantErrorCode: "server_error",
			wantErrorText: "inference failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					`data: {"type":"`+test.eventType+`","response":{"id":"resp_terminal","object":"response","created_at":1,"status":"`+test.status+`","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}`+test.extra+`}}`+"\n\n"+
						`data: {"type":"response.completed","response":{"id":"must_not_pass","object":"response","created_at":2,"status":"completed","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			defer upstream.Close()

			route := facadeRoute("deepseek-terminal", "responses", "deepseek-v4-pro", "key", upstream.URL)
			route.Host = "api.deepseek.com"
			route.ChatSearchDialect = config.ChatSearchDialectResponses
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", true), "")
			if status != http.StatusOK || bytes.Contains(data, []byte("must_not_pass")) {
				t.Fatalf("status=%d terminal stream was not bounded: %s", status, data)
			}
			var terminal map[string]any
			err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
				event, err := decodeJSONMap(payload)
				if err != nil {
					return err
				}
				if stringValue(event["type"]) == test.eventType {
					terminal, _ = event["response"].(map[string]any)
				}
				return nil
			})
			if err != nil || terminal == nil {
				t.Fatalf("terminal event missing or invalid: err=%v body=%s", err, data)
			}
			if terminal["status"] != test.status {
				t.Fatalf("terminal status changed: %#v", terminal)
			}
			if test.wantDetail != "" {
				details, _ := terminal["incomplete_details"].(map[string]any)
				if details["reason"] != test.wantDetail || terminal["error"] != nil {
					t.Fatalf("incomplete details changed: %#v", terminal)
				}
			}
			if test.wantErrorCode != "" {
				failure, _ := terminal["error"].(map[string]any)
				if failure["code"] != test.wantErrorCode || failure["message"] != test.wantErrorText || terminal["incomplete_details"] != nil {
					t.Fatalf("failure details changed: %#v", terminal)
				}
			}
			usage, _ := terminal["usage"].(map[string]any)
			contextDetails, _ := usage["context_details"].(map[string]any)
			if numberInt(usage["total_tokens"]) != 13 || numberInt(contextDetails["input_tokens"]) != 9 ||
				numberInt(contextDetails["output_tokens"]) != 4 {
				t.Fatalf("terminal usage changed: %#v", usage)
			}
		})
	}
}

func TestDeepSeekResponsesExposeLiveContextToGrokBuild(t *testing.T) {
	tests := []struct {
		name     string
		stream   bool
		official bool
	}{
		{name: "official JSON", official: true},
		{name: "official SSE", stream: true, official: true},
		{name: "generic relay remains untouched"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":40,"output_tokens":2,"total_tokens":99}}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":40,"output_tokens":2,"total_tokens":99}}`)
			}))
			defer upstream.Close()

			route := facadeRoute("deepseek-context", "responses", "deepseek-v4-pro", "key", upstream.URL)
			if test.official {
				route.Host = "api.deepseek.com"
				route.ChatSearchDialect = config.ChatSearchDialectResponses
			}
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", test.stream), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			var response map[string]any
			if test.stream {
				err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
					event, err := decodeJSONMap(payload)
					if err != nil {
						return err
					}
					if stringValue(event["type"]) == "response.completed" {
						response, _ = event["response"].(map[string]any)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				response, err = decodeJSONMap(data)
				if err != nil {
					t.Fatal(err)
				}
			}
			if response == nil {
				t.Fatalf("terminal response missing: %s", data)
			}
			usage, _ := response["usage"].(map[string]any)
			contextDetails, hasContext := usage["context_details"].(map[string]any)
			if !hasContext {
				t.Fatalf("context_details missing: %s", data)
			}
			if numberInt(usage["total_tokens"]) != 99 {
				t.Fatalf("billing total changed: %s", data)
			}
			if numberInt(contextDetails["input_tokens"]) != 40 || numberInt(contextDetails["output_tokens"]) != 2 {
				t.Fatalf("live context details are wrong: %s", data)
			}
		})
	}
}

func TestDeepSeekResponsesPreserveProviderUsageExtensions(t *testing.T) {
	tests := []struct {
		name           string
		stream         bool
		contextDetails string
		wantOutput     bool
	}{
		{
			name:           "JSON complete context details",
			contextDetails: `{"input_tokens":700,"output_tokens":8,"provider_field":true}`,
			wantOutput:     true,
		},
		{
			name:           "JSON partial context details",
			contextDetails: `{"input_tokens":700,"provider_field":true}`,
		},
		{
			name:           "SSE complete context details",
			stream:         true,
			contextDetails: `{"input_tokens":700,"output_tokens":8,"provider_field":true}`,
			wantOutput:     true,
		},
		{
			name:           "SSE partial context details",
			stream:         true,
			contextDetails: `{"input_tokens":700,"provider_field":true}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := `{"input_tokens":40,"output_tokens":2,"total_tokens":99,` +
				`"input_tokens_details":{"cached_tokens":30,"future_cache_tokens":7},` +
				`"output_tokens_details":{"reasoning_tokens":1,"future_reasoning_tokens":3},` +
				`"future_usage":{"billable_tokens":88},"context_details":` + test.contextDetails + `}`
			responseBody := `{"id":"resp_usage_extensions","object":"response","created_at":1,` +
				`"status":"completed","model":"deepseek-v4-pro","output":[],"usage":` + usage + `}`

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if test.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":`+responseBody+`}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, responseBody)
			}))
			defer upstream.Close()

			route := facadeRoute("deepseek-usage-extensions", "responses", "deepseek-v4-pro", "key", upstream.URL)
			route.Host = "api.deepseek.com"
			route.ChatSearchDialect = config.ChatSearchDialectResponses
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", test.stream), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			var response map[string]any
			if test.stream {
				err := scanSSEPayloads(bytes.NewReader(data), func(_ []string, payload []byte) error {
					event, decodeErr := decodeJSONMap(payload)
					if decodeErr != nil {
						return decodeErr
					}
					if stringValue(event["type"]) == "response.completed" {
						response, _ = event["response"].(map[string]any)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var err error
				response, err = decodeJSONMap(data)
				if err != nil {
					t.Fatal(err)
				}
			}

			usageResult, _ := response["usage"].(map[string]any)
			inputDetails, _ := usageResult["input_tokens_details"].(map[string]any)
			outputDetails, _ := usageResult["output_tokens_details"].(map[string]any)
			futureUsage, _ := usageResult["future_usage"].(map[string]any)
			contextDetails, _ := usageResult["context_details"].(map[string]any)
			_, hasOutput := contextDetails["output_tokens"]
			if numberInt(inputDetails["cached_tokens"]) != 30 || numberInt(inputDetails["future_cache_tokens"]) != 7 ||
				numberInt(outputDetails["reasoning_tokens"]) != 1 || numberInt(outputDetails["future_reasoning_tokens"]) != 3 ||
				numberInt(futureUsage["billable_tokens"]) != 88 || numberInt(contextDetails["input_tokens"]) != 700 ||
				contextDetails["provider_field"] != true || hasOutput != test.wantOutput {
				t.Fatalf("provider usage extensions changed: %#v", usageResult)
			}
			if test.wantOutput && numberInt(contextDetails["output_tokens"]) != 8 {
				t.Fatalf("provider context output changed: %#v", contextDetails)
			}
		})
	}
}

func TestDeepSeekInsufficientSystemResourceDetectionIsScoped(t *testing.T) {
	root := map[string]any{"choices": []any{map[string]any{"finish_reason": "insufficient_system_resource"}}}
	official := config.Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro", APIBackend: "chat_completions"}
	if !deepSeekChatInsufficientSystemResource(root, official, wireChatCompletions) {
		t.Fatal("official DeepSeek V4 Chat resource failure was not detected")
	}
	if deepSeekChatInsufficientSystemResource(root, official, wireResponses) {
		t.Fatal("Responses protocol was treated as Chat Completions")
	}
	relay := official
	relay.Host = "relay.example"
	if deepSeekChatInsufficientSystemResource(root, relay, wireChatCompletions) {
		t.Fatal("relay response received first-party DeepSeek failure semantics")
	}
}
