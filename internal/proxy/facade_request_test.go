package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestChannelPathsExposeAllNativeProtocols(t *testing.T) {
	tests := []struct {
		path     string
		channel  string
		protocol wireProtocol
		valid    bool
	}{
		{"/c/model/responses", "model", wireResponses, true},
		{"/c/model/messages", "model", wireMessages, true},
		{"/c/model/chat/completions", "model", wireChatCompletions, true},
		{"/c/provider%2Fmodel/messages", "provider/model", wireMessages, true},
		{"/c/model/completions", "", wireUnknown, false},
		{"/c/model/responses/extra", "", wireUnknown, false},
	}
	for _, test := range tests {
		channel, protocol, ok := channelFromPath(test.path)
		if ok != test.valid || channel != test.channel || protocol != test.protocol {
			t.Fatalf("path=%q got channel=%q protocol=%q ok=%t", test.path, channel, protocol, ok)
		}
	}
}

func TestGrokBuildLocalToolsSurviveEveryProviderBridge(t *testing.T) {
	localNames := []string{"shell", "read_file", "apply_patch", "task", "mcp__example__lookup"}
	tools := make([]any, 0, len(localNames))
	for _, name := range localNames {
		tools = append(tools, map[string]any{
			"type": "function", "name": name, "description": "Grok Build local tool",
			"parameters": map[string]any{"type": "object"},
		})
	}
	body, err := json.Marshal(map[string]any{
		"model":             "display",
		"max_output_tokens": 4096,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "run the local tool"},
			map[string]any{
				"type": "function_call", "id": "fc_shell", "call_id": "call_shell",
				"name": "shell", "arguments": `{"command":"pwd"}`, "status": "completed",
			},
			map[string]any{"type": "function_call_output", "call_id": "call_shell", "output": "D:/repo"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
		"tools":       tools,
		"tool_choice": map[string]any{"type": "function", "name": "task"},
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name     string
		backend  string
		protocol wireProtocol
		dialect  config.ChatSearchDialect
	}{
		{name: "responses", backend: "responses", protocol: wireResponses},
		{name: "messages", backend: "messages", protocol: wireMessages},
		{name: "chat", backend: "chat_completions", protocol: wireChatCompletions, dialect: config.ChatSearchDialectWebSearchOptions},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			route := config.Route{
				ChannelID: "tools", WireModel: "wire", APIBackend: test.backend, APIBackendConfigured: true,
				SupportsBackendSearch: true, ChatSearchDialect: test.dialect,
			}
			request, err := adaptFacadeRequest(body, route, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.Protocol != test.protocol {
				t.Fatalf("provider protocol=%s want %s", request.Protocol, test.protocol)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}

			declared := map[string]bool{}
			selected := ""
			sawCall := false
			sawResult := false
			switch test.protocol {
			case wireResponses:
				for _, raw := range anySlice(root["tools"]) {
					tool, _ := raw.(map[string]any)
					if stringValue(tool["type"]) == "function" {
						declared[functionToolName(tool)] = true
					}
				}
				choice, _ := root["tool_choice"].(map[string]any)
				selected = functionToolName(choice)
				for _, raw := range anySlice(root["input"]) {
					item, _ := raw.(map[string]any)
					sawCall = sawCall || (stringValue(item["type"]) == "function_call" && stringValue(item["name"]) == "shell")
					sawResult = sawResult || (stringValue(item["type"]) == "function_call_output" && stringValue(item["call_id"]) == "call_shell")
				}
			case wireMessages:
				for _, raw := range anySlice(root["tools"]) {
					tool, _ := raw.(map[string]any)
					if tool["input_schema"] != nil {
						declared[stringValue(tool["name"])] = true
					}
				}
				choice, _ := root["tool_choice"].(map[string]any)
				selected = stringValue(choice["name"])
				for _, rawMessage := range anySlice(root["messages"]) {
					message, _ := rawMessage.(map[string]any)
					for _, rawBlock := range anySlice(message["content"]) {
						block, _ := rawBlock.(map[string]any)
						sawCall = sawCall || (stringValue(block["type"]) == "tool_use" && stringValue(block["name"]) == "shell")
						sawResult = sawResult || (stringValue(block["type"]) == "tool_result" && stringValue(block["tool_use_id"]) == "call_shell")
					}
				}
			case wireChatCompletions:
				for _, raw := range anySlice(root["tools"]) {
					tool, _ := raw.(map[string]any)
					if stringValue(tool["type"]) == "function" {
						declared[functionToolName(tool)] = true
					}
				}
				choice, _ := root["tool_choice"].(map[string]any)
				selected = functionToolName(choice)
				for _, rawMessage := range anySlice(root["messages"]) {
					message, _ := rawMessage.(map[string]any)
					if stringValue(message["role"]) == "tool" && stringValue(message["tool_call_id"]) == "call_shell" {
						sawResult = true
					}
					for _, rawCall := range anySlice(message["tool_calls"]) {
						call, _ := rawCall.(map[string]any)
						function, _ := call["function"].(map[string]any)
						sawCall = sawCall || stringValue(function["name"]) == "shell"
					}
				}
			}
			for _, name := range localNames {
				if !declared[name] {
					t.Fatalf("local tool %q was not declared on %s wire: %s", name, test.name, request.Body)
				}
			}
			if selected != "task" || !sawCall || !sawResult {
				t.Fatalf("selection/call/replay lost on %s: selected=%q call=%t result=%t body=%s",
					test.name, selected, sawCall, sawResult, request.Body)
			}
		})
	}
}

func TestDeepSeekTargetsPreserveBasePathPrefixes(t *testing.T) {
	tests := []struct {
		name     string
		origin   string
		protocol wireProtocol
		want     string
	}{
		{"Responses root", "https://api.deepseek.com", wireResponses, "https://api.deepseek.com/responses"},
		{"Responses OpenAI root", "https://api.deepseek.com/v1", wireResponses, "https://api.deepseek.com/responses"},
		{"Responses Anthropic root", "https://api.deepseek.com/anthropic", wireResponses, "https://api.deepseek.com/responses"},
		{"Messages root", "https://api.deepseek.com", wireMessages, "https://api.deepseek.com/anthropic/v1/messages"},
		{"Messages Anthropic root", "https://api.deepseek.com/anthropic/v1", wireMessages, "https://api.deepseek.com/anthropic/v1/messages"},
		{"Chat Anthropic root", "https://api.deepseek.com/anthropic", wireChatCompletions, "https://api.deepseek.com/chat/completions"},
		{"Responses custom prefix", "https://api.deepseek.com/gateway/tenant/v1", wireResponses, "https://api.deepseek.com/gateway/tenant/responses"},
		{"Messages custom prefix", "https://api.deepseek.com/gateway/tenant/anthropic", wireMessages, "https://api.deepseek.com/gateway/tenant/anthropic/v1/messages"},
		{"Chat versioned prefix", "https://api.deepseek.com/api/2026-08/anthropic/v1", wireChatCompletions, "https://api.deepseek.com/api/2026-08/chat/completions"},
		{"Unknown future prefix", "https://api.deepseek.com/experimental/v2", wireResponses, "https://api.deepseek.com/experimental/v2/responses"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target, err := upstreamTarget(config.Route{
				Host:       "api.deepseek.com",
				OriginBase: test.origin,
			}, test.protocol, "")
			if err != nil {
				t.Fatal(err)
			}
			if target != test.want {
				t.Fatalf("target=%q want=%q", target, test.want)
			}
		})
	}
}

func TestNonDeepSeekTargetKeepsProviderBasePath(t *testing.T) {
	target, err := upstreamTarget(config.Route{
		Host: "relay.example", OriginBase: "https://relay.example/tenant/v1",
	}, wireMessages, "")
	if err != nil {
		t.Fatal(err)
	}
	if target != "https://relay.example/tenant/v1/messages" {
		t.Fatalf("target=%q", target)
	}
}

func TestNativeSessionRequestsStayInTheirOwnProtocol(t *testing.T) {
	tests := []struct {
		backend string
		body    string
		field   string
	}{
		{"responses", `{"model":"display","input":[{"role":"user","content":"hi"}],"stream":false}`, "input"},
		{"messages", `{"model":"display","messages":[{"role":"user","content":"hi"}],"max_tokens":512,"stream":false}`, "messages"},
		{"chat_completions", `{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":false}`, "messages"},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			protocol := backendProtocol(t, test.backend)
			request, err := adaptFacadeRequest([]byte(test.body), config.Route{
				ChannelID:  test.backend,
				APIBackend: test.backend,
				WireModel:  "wire-model",
			}, protocol)
			if err != nil {
				t.Fatal(err)
			}
			if request.Kind != nativeSessionRequest || request.IncomingProtocol != protocol || request.Protocol != protocol {
				t.Fatalf("request classification changed: %#v", request)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if root["model"] != "wire-model" || root[test.field] == nil {
				t.Fatalf("native request was not preserved: %#v", root)
			}
			if test.backend == "responses" && root["messages"] != nil {
				t.Fatalf("Responses request gained Messages history: %#v", root)
			}
			if test.backend != "responses" && root["input"] != nil {
				t.Fatalf("%s request gained Responses input: %#v", test.backend, root)
			}
		})
	}
}

func TestUnconfiguredBackendFollowsGrokBuildResolvedProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol wireProtocol
		body     string
	}{
		{"remote Responses", wireResponses, `{"model":"catalog-model","input":"hi","stream":false}`},
		{"remote Messages", wireMessages, `{"model":"catalog-model","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"stream":false}`},
		{"remote Chat Completions", wireChatCompletions, `{"model":"catalog-model","messages":[{"role":"user","content":"hi"}],"stream":false}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest([]byte(test.body), config.Route{
				ChannelID: "future", WireModel: "future-provider-model",
			}, test.protocol)
			if err != nil {
				t.Fatal(err)
			}
			if request.IncomingProtocol != test.protocol || request.Protocol != test.protocol {
				t.Fatalf("remote protocol was not followed: %#v", request)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if root["model"] != "future-provider-model" {
				t.Fatalf("wire model was not isolated: %#v", root)
			}
		})
	}
}

func TestRemoteCatalogHostedSearchIsDetectedWithoutLocalModelKnowledge(t *testing.T) {
	body := []byte(`{
		"model":"catalog-alias",
		"input":[{"role":"user","content":"find current docs"}],
		"max_output_tokens":4096,
		"tools":[
			{"type":"web_search"},
			{"type":"function","name":"skill","description":"load a skill","parameters":{"type":"object"}}
		],
		"reasoning":{"effort":"medium"},
		"stream":false
	}`)
	for _, test := range []struct {
		name     string
		backend  string
		protocol wireProtocol
	}{
		{name: "Responses upstream", backend: "responses", protocol: wireResponses},
		{name: "Messages upstream", backend: "messages", protocol: wireMessages},
		{name: "Chat Completions upstream", backend: "chat_completions", protocol: wireChatCompletions},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, config.Route{
				ChannelID: "future-provider", Host: "relay.example",
				OriginBase: "https://relay.example/v1", WireModel: "future-provider-model",
				APIBackend: test.backend, APIBackendConfigured: true,
			}, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.Kind != nativeSessionRequest || request.Protocol != test.protocol ||
				!request.HostedWebSearch || request.ProxyAddedWebSearch {
				t.Fatalf("remote hosted-search capability was not inferred from the request: %#v", request)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !nativeRequestHasSearchForTest(root, test.protocol) {
				t.Fatalf("hosted web_search did not reach %s upstream: %#v", test.backend, root)
			}
			foundSkill := false
			for _, raw := range anySlice(root["tools"]) {
				tool, _ := raw.(map[string]any)
				foundSkill = foundSkill || functionToolName(tool) == "skill"
			}
			if !foundSkill {
				t.Fatalf("Grok Build function tool was lost during %s conversion: %#v", test.backend, root)
			}
		})
	}
}

func TestThirdPartyRoutesNeverReceiveXSearch(t *testing.T) {
	body := []byte(`{
		"model":"catalog-alias",
		"input":[{"role":"user","content":"find current docs"}],
		"max_output_tokens":4096,
		"tools":[
			{"type":"web_search","filters":{"allowed_domains":["example.test"]}},
			{"type":"x_search","from_date":"2026-01-01"},
			{"type":"function","name":"read_file","description":"read a file","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","tools":[{"type":"web_search"},{"type":"x_search"},{"type":"function","name":"read_file"}]},
		"stream":false
	}`)
	for _, test := range []struct {
		name     string
		backend  string
		protocol wireProtocol
	}{
		{name: "Responses", backend: "responses", protocol: wireResponses},
		{name: "Messages", backend: "messages", protocol: wireMessages},
		{name: "Chat Completions", backend: "chat_completions", protocol: wireChatCompletions},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, config.Route{
				ChannelID: "third-party", Host: "relay.example", WireModel: "provider-model",
				APIBackend: test.backend, APIBackendConfigured: true,
			}, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.Protocol != test.protocol || bytes.Contains(request.Body, []byte(`"x_search"`)) {
				t.Fatalf("third-party wire retained x_search: protocol=%s body=%s", request.Protocol, request.Body)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !nativeRequestHasSearchForTest(root, test.protocol) {
				t.Fatalf("web_search was lost while removing x_search: %s", request.Body)
			}
			foundReadFile := false
			for _, raw := range anySlice(root["tools"]) {
				tool, _ := raw.(map[string]any)
				foundReadFile = foundReadFile || functionToolName(tool) == "read_file"
			}
			if !foundReadFile {
				t.Fatalf("local function tool was lost: %s", request.Body)
			}
		})
	}
}

func TestCrossProtocolResponsesEndpointAcceptsOnlyBuildClientSearch(t *testing.T) {
	for _, backend := range []string{"messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			route := config.Route{ChannelID: backend, APIBackend: backend, WireModel: "wire"}
			_, err := adaptFacadeRequest([]byte(`{"input":"ordinary conversation"}`), route, wireResponses)
			if err == nil || !strings.Contains(err.Error(), "expects") {
				t.Fatalf("ordinary cross-protocol request was accepted: %v", err)
			}
			request, err := adaptFacadeRequest(buildClientSearchBody("search this"), route, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.Kind != clientSearchRequest || request.Protocol != backendProtocol(t, backend) || request.Stream {
				t.Fatalf("search request classification=%#v", request)
			}
		})
	}
}

func TestBuildClientSearchFingerprintUsesStableRequestShape(t *testing.T) {
	base, err := decodeRequestObject(buildClientSearchBody("q"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"streaming", func(root map[string]any) { root["stream"] = true }},
		{"stored", func(root map[string]any) { root["store"] = true }},
		{"missing input", func(root map[string]any) { delete(root, "input") }},
		{"array input", func(root map[string]any) { root["input"] = []any{map[string]any{"role": "user", "content": "q"}} }},
		{"empty input", func(root map[string]any) { root["input"] = "  " }},
		{"missing store", func(root map[string]any) { delete(root, "store") }},
		{"disabled tool choice", func(root map[string]any) { root["tool_choice"] = "none" }},
		{"non-search tool", func(root map[string]any) {
			root["tools"] = []any{map[string]any{"type": "function", "name": "lookup"}}
		}},
		{"extra tool", func(root map[string]any) {
			root["tools"] = append(anySlice(root["tools"]), map[string]any{"type": "function", "name": "x"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := cloneMap(base)
			test.mutate(root)
			hosted, x := summarizeSearchCounts(root)
			if prepareClientSearchExecution(root, hosted, x) {
				t.Fatal("near-match request was classified as WebSearchClient")
			}
		})
	}

	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"sampling parameters omitted", func(root map[string]any) {
			delete(root, "temperature")
			delete(root, "top_p")
			delete(root, "max_output_tokens")
		}},
		{"sampling parameters changed", func(root map[string]any) {
			root["temperature"] = 0.2
			root["top_p"] = 0.9
			root["max_output_tokens"] = 16384
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := cloneMap(base)
			test.mutate(root)
			hosted, x := summarizeSearchCounts(root)
			if !prepareClientSearchExecution(root, hosted, x) {
				t.Fatal("WebSearchClient request stopped matching after an incidental sampling change")
			}
		})
	}
}

func TestBuildClientSearchPreservesAutomaticToolChoiceForFinalText(t *testing.T) {
	root, err := decodeRequestObject(buildClientSearchBody("current news"))
	if err != nil {
		t.Fatal(err)
	}
	hosted, x := summarizeSearchCounts(root)
	if !prepareClientSearchExecution(root, hosted, x) {
		t.Fatal("WebSearchClient request was not recognized")
	}
	if _, exists := root["tool_choice"]; exists {
		t.Fatalf("automatic tool choice was forced into a server-tool loop: %#v", root["tool_choice"])
	}
	if !strings.Contains(stringValue(root["instructions"]), "always return a concise final text synthesis") {
		t.Fatalf("client search synthesis instruction missing: %#v", root["instructions"])
	}
}

func TestSearchAdapterMapsMessagesAndChatDialects(t *testing.T) {
	body := buildClientSearchBodyWithTool("q", map[string]any{
		"type":          "web_search",
		"filters":       map[string]any{"allowed_domains": []any{"example.com", "docs.example.com"}},
		"user_location": map[string]any{"type": "approximate", "country": "CN"},
	})

	messages, err := adaptFacadeRequest(body, config.Route{
		ChannelID: "claude", APIBackend: "messages", WireModel: "claude-wire",
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	messagesRoot, _ := decodeRequestObject(messages.Body)
	messageTool := anySlice(messagesRoot["tools"])[0].(map[string]any)
	if messageTool["type"] != "web_search_20250305" || messageTool["name"] != "web_search" ||
		len(anySlice(messageTool["allowed_domains"])) != 2 {
		t.Fatalf("Messages search mapping=%#v", messagesRoot)
	}

	openAI, err := adaptFacadeRequest(body, config.Route{
		ChannelID: "gpt", APIBackend: "chat_completions", WireModel: "gpt-wire", Host: "relay.example",
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	openAIRoot, _ := decodeRequestObject(openAI.Body)
	if _, ok := openAIRoot["web_search_options"].(map[string]any); !ok || openAIRoot["search_parameters"] != nil {
		t.Fatalf("OpenAI-compatible search dialect=%#v", openAIRoot)
	}

	xai, err := adaptFacadeRequest(body, config.Route{
		ChannelID: "relay-with-arbitrary-name", APIBackend: "chat_completions", WireModel: "arbitrary-model",
		Host: "api.x.ai", ChatSearchDialect: config.ChatSearchDialectSearchParameters,
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	xaiRoot, _ := decodeRequestObject(xai.Body)
	parameters, _ := xaiRoot["search_parameters"].(map[string]any)
	sources := anySlice(parameters["sources"])
	if parameters["mode"] != "auto" || len(sources) != 1 ||
		len(anySlice(sources[0].(map[string]any)["allowed_websites"])) != 2 {
		t.Fatalf("xAI search dialect=%#v", xaiRoot)
	}
}

func TestResponsesToMessagesDoesNotInventMaxOutputTokens(t *testing.T) {
	_, err := responsesToMessagesRequest(map[string]any{
		"model": "display",
		"input": "search this",
	})
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens must be a positive integer") {
		t.Fatalf("missing max_output_tokens error=%v", err)
	}
}

func TestChatSearchDialectUsesExplicitOrOfficialHostCapabilities(t *testing.T) {
	for _, route := range []config.Route{
		{ChannelID: "grok-channel", WireModel: "ordinary", Host: "relay.example"},
		{ChannelID: "ordinary", WireModel: "grok-9", Host: "relay.example"},
	} {
		if got := chatSearchDialect(route); got != config.ChatSearchDialectWebSearchOptions {
			t.Fatalf("route=%#v guessed dialect %q", route, got)
		}
	}
	if got := chatSearchDialect(config.Route{Host: "api.x.ai"}); got != config.ChatSearchDialectResponses {
		t.Fatalf("official xAI host default=%q", got)
	}
	if got := chatSearchDialect(config.Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro"}); got != config.ChatSearchDialectResponses {
		t.Fatalf("official DeepSeek V4 host default=%q", got)
	}
	if got := chatSearchDialect(config.Route{Host: "api.deepseek.com", WireModel: "deepseek-future-model"}); got != config.ChatSearchDialectResponses {
		t.Fatalf("future official DeepSeek model default=%q", got)
	}
	explicit := config.Route{Host: "api.deepseek.com", ChatSearchDialect: config.ChatSearchDialectWebSearchOptions}
	if got := chatSearchDialect(explicit); got != config.ChatSearchDialectWebSearchOptions {
		t.Fatalf("explicit dialect was ignored: %q", got)
	}
	if got := chatSearchDialect(config.Route{Host: "preview.deepseek.com", WireModel: "deepseek-v4-pro"}); got != config.ChatSearchDialectWebSearchOptions {
		t.Fatalf("undocumented DeepSeek subdomain received first-party assumptions: %q", got)
	}
}

func TestProviderSearchProtocolHonorsConfiguredNativeAPIAndExplicitChatDialect(t *testing.T) {
	tests := []struct {
		name  string
		route config.Route
		want  wireProtocol
	}{
		{
			name:  "official Responses remains native without request evidence",
			route: config.Route{Host: "api.deepseek.com", APIBackend: "responses", WireModel: "deepseek-v4-pro"},
			want:  wireResponses,
		},
		{
			name:  "official Messages stays Messages",
			route: config.Route{Host: "api.deepseek.com", APIBackend: "messages", WireModel: "deepseek-v4-pro[1m]"},
			want:  wireMessages,
		},
		{
			name:  "official Chat uses documented Responses default",
			route: config.Route{Host: "api.deepseek.com", APIBackend: "chat_completions", WireModel: "deepseek-future-model"},
			want:  wireResponses,
		},
		{
			name: "explicit Chat dialect overrides host default",
			route: config.Route{
				Host: "api.deepseek.com", APIBackend: "chat_completions", WireModel: "deepseek-future-model",
				ChatSearchDialect: config.ChatSearchDialectWebSearchOptions,
			},
			want: wireChatCompletions,
		},
		{
			name: "explicit Messages bridge overrides host default",
			route: config.Route{
				Host: "api.deepseek.com", APIBackend: "chat_completions", WireModel: "deepseek-future-model",
				ChatSearchDialect: config.ChatSearchDialectMessages,
			},
			want: wireMessages,
		},
		{
			name: "explicit Responses search dialect opts out of host default",
			route: config.Route{
				Host: "api.deepseek.com", APIBackend: "responses", WireModel: "deepseek-future-model",
				ChatSearchDialect: config.ChatSearchDialectResponses,
			},
			want: wireResponses,
		},
		{
			name: "explicit Messages search dialect bridges Responses",
			route: config.Route{
				Host: "api.deepseek.com", APIBackend: "responses", WireModel: "deepseek-future-model",
				ChatSearchDialect: config.ChatSearchDialectMessages,
			},
			want: wireMessages,
		},
		{
			name: "explicit Responses search dialect bridges Messages",
			route: config.Route{
				Host: "api.deepseek.com", APIBackend: "messages", WireModel: "deepseek-future-model",
				ChatSearchDialect: config.ChatSearchDialectResponses,
			},
			want: wireResponses,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := providerSearchProtocol(test.route)
			if err != nil || got != test.want {
				t.Fatalf("protocol=%s want=%s err=%v", got, test.want, err)
			}
		})
	}
}

func TestDeepSeekSearchHistoryRepairRequiresOfficialResponsesRoute(t *testing.T) {
	body := []byte(`{
		"input":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"current"}}],
		"tools":[{"type":"web_search"}]
	}`)
	tests := []struct {
		name       string
		route      config.Route
		wantRepair bool
	}{
		{
			name: "official future model",
			route: config.Route{
				ChannelID: "future", Host: "api.deepseek.com", APIBackend: "responses",
				WireModel: "deepseek-future-model", SupportsBackendSearch: true,
				ChatSearchDialect: config.ChatSearchDialectResponses,
			},
			wantRepair: true,
		},
		{
			name: "relay reusing DeepSeek model ID",
			route: config.Route{
				ChannelID: "relay", Host: "relay.example", APIBackend: "responses",
				WireModel: "deepseek-v4-pro", SupportsBackendSearch: true,
			},
			wantRepair: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, test.route, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			input := anySlice(root["input"])
			action, _ := input[0].(map[string]any)["action"].(map[string]any)
			_, repaired := action["queries"]
			if repaired != test.wantRepair {
				t.Fatalf("queries repaired=%t want=%t body=%s", repaired, test.wantRepair, request.Body)
			}
		})
	}
}

func TestResponsesHostedSearchIsProtocolLocal(t *testing.T) {
	route := config.Route{
		ChannelID: "responses", APIBackend: "responses", WireModel: "wire",
		SupportsBackendSearch: true, Host: "relay.example",
	}
	request, err := adaptFacadeRequest([]byte(`{"input":"hi","tools":[{"type":"function","name":"skill","parameters":{"type":"object"}}]}`), route, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := decodeRequestObject(request.Body)
	if !request.ProxyAddedWebSearch || !request.HostedWebSearch || !hasHostedSearchTool(root) {
		t.Fatalf("Responses hosted search was not normalized: %#v %#v", request, root)
	}
	if request.BuildXSearch != 0 || summarizeXSearch(root) != 0 {
		t.Fatalf("non-xAI route gained x_search: %#v", root)
	}
	includes := anySlice(root["include"])
	if len(includes) != 1 || includes[0] != responsesWebSearchSourcesInclude {
		t.Fatalf("Responses hosted search did not request source metadata: %#v", root)
	}
}

func TestOfficialDeepSeekKeepsConfiguredProtocolWhenToolChoiceExcludesSearch(t *testing.T) {
	for _, test := range []struct {
		backend string
		want    wireProtocol
	}{
		{backend: "responses", want: wireResponses},
		{backend: "messages", want: wireMessages},
		{backend: "chat_completions", want: wireChatCompletions},
	} {
		t.Run(test.backend, func(t *testing.T) {
			route := config.Route{
				ChannelID: "deepseek", Host: "api.deepseek.com", APIBackend: test.backend,
				WireModel: "deepseek-future-model", SupportsBackendSearch: true,
			}
			request, err := adaptFacadeRequest([]byte(`{
				"input":"return structured output",
				"max_output_tokens":128,
				"reasoning":{"effort":"high"},
				"tools":[
					{"type":"function","name":"structured","parameters":{"type":"object"}},
					{"type":"web_search"}
				],
				"tool_choice":{"type":"function","name":"structured"}
			}`), route, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.Protocol != test.want || request.ProxyAddedWebSearch || !request.HostedWebSearch {
				t.Fatalf("non-search DeepSeek request changed protocol: %#v", request)
			}
			root, err := decodeRequestObject(request.Body)
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(root["tools"])
			if !bytes.Contains(encoded, []byte(`"structured"`)) {
				t.Fatalf("configured protocol lost the ordinary function declaration: %#v", root)
			}
			if test.want == wireChatCompletions && (root["web_search_options"] != nil || root["search_parameters"] != nil) {
				t.Fatalf("excluded search gained a Chat hosted-search extension: %#v", root)
			}
		})
	}
}

func TestResponsesHostedSearchPreservesExistingIncludes(t *testing.T) {
	body := []byte(`{
		"input":"search current news",
		"tools":[{"type":"web_search"}],
		"include":["reasoning.encrypted_content"]
	}`)
	request, err := adaptFacadeRequest(body, config.Route{
		ChannelID: "responses", APIBackend: "responses", WireModel: "wire",
		SupportsBackendSearch: true,
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := decodeRequestObject(request.Body)
	includes := anySlice(root["include"])
	if len(includes) != 2 || includes[0] != "reasoning.encrypted_content" ||
		includes[1] != responsesWebSearchSourcesInclude {
		t.Fatalf("Responses include list changed: %#v", includes)
	}
	if includeResponsesWebSearchSources(root) {
		t.Fatal("Responses source metadata was appended twice")
	}
	if includes = anySlice(root["include"]); len(includes) != 2 {
		t.Fatalf("Responses include list contains duplicates: %#v", includes)
	}

	ordinary, err := adaptFacadeRequest([]byte(`{"input":"hello"}`), config.Route{
		ChannelID: "responses", APIBackend: "responses", WireModel: "wire",
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	ordinaryRoot, _ := decodeRequestObject(ordinary.Body)
	if ordinaryRoot["include"] != nil {
		t.Fatalf("ordinary Responses request gained search metadata: %#v", ordinaryRoot)
	}
}

func TestCapableMessagesConsumesResponsesAndUsesHostedSearch(t *testing.T) {
	body := []byte(`{
		"model":"display",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search current news"}]}],
		"max_output_tokens":4096,
		"stream":true,
		"tools":[
			{"type":"function","name":"web_fetch","parameters":{"type":"object"}},
			{"type":"function","name":"skill","parameters":{"type":"object"}}
		]
	}`)
	request, err := adaptFacadeRequest(body, config.Route{
		ChannelID: "messages", APIBackend: "messages", WireModel: "wire", SupportsBackendSearch: true,
	}, wireResponses)
	if err != nil {
		t.Fatal(err)
	}
	if request.IncomingProtocol != wireResponses || request.Protocol != wireMessages ||
		!request.HostedWebSearch || !request.ProxyAddedWebSearch || request.ClientSearchAlias != "" {
		t.Fatalf("Messages search routing=%#v", request)
	}
	root, _ := decodeRequestObject(request.Body)
	if root["input"] != nil || root["messages"] == nil || root["stream"] != true {
		t.Fatalf("Responses request was not converted: %#v", root)
	}
	tools := anySlice(root["tools"])
	var hosted, fetch, skill bool
	for _, raw := range tools {
		tool := raw.(map[string]any)
		switch {
		case tool["type"] == "web_search_20250305" && tool["name"] == "web_search":
			hosted = true
		case functionToolName(tool) == "web_fetch":
			fetch = true
		case functionToolName(tool) == "skill":
			skill = true
		}
	}
	if !hosted || !fetch || !skill {
		t.Fatalf("Messages tools were not preserved and promoted: %#v", tools)
	}
}

func TestCapableChatConsumesResponsesAndUsesConfiguredDialect(t *testing.T) {
	body := []byte(`{
		"model":"display","input":"search current news","stream":true,
		"tools":[{"type":"function","name":"web_fetch","parameters":{"type":"object"}}],
		"tool_choice":{"type":"web_search"}
	}`)
	for _, test := range []struct {
		name    string
		route   config.Route
		field   string
		missing string
	}{
		{
			name:    "OpenAI-compatible web_search_options",
			route:   config.Route{APIBackend: "chat_completions", WireModel: "wire", Host: "relay.example", SupportsBackendSearch: true},
			field:   "web_search_options",
			missing: "search_parameters",
		},
		{
			name: "xAI search_parameters",
			route: config.Route{
				APIBackend: "chat_completions", WireModel: "wire", Host: "relay.example", SupportsBackendSearch: true,
				ChatSearchDialect: config.ChatSearchDialectSearchParameters,
			},
			field:   "search_parameters",
			missing: "web_search_options",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := adaptFacadeRequest(body, test.route, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.IncomingProtocol != wireResponses || request.Protocol != wireChatCompletions ||
				!request.HostedWebSearch || !request.ProxyAddedWebSearch || request.ClientSearchAlias != "" {
				t.Fatalf("Chat search routing=%#v", request)
			}
			root, _ := decodeRequestObject(request.Body)
			if root[test.field] == nil || root[test.missing] != nil || root["input"] != nil || root["messages"] == nil {
				t.Fatalf("Chat hosted search mapping=%#v", root)
			}
			tools := anySlice(root["tools"])
			if len(tools) != 1 || functionToolName(tools[0].(map[string]any)) != "web_fetch" {
				t.Fatalf("web_fetch changed: %#v", tools)
			}
			if test.field == "search_parameters" {
				parameters := root[test.field].(map[string]any)
				if parameters["mode"] != "on" || len(anySlice(parameters["sources"])) != 1 {
					t.Fatalf("search_parameters=%#v", parameters)
				}
			}
		})
	}
}

func TestCapableHostedSearchRespectsStructuredToolChoice(t *testing.T) {
	tests := []struct {
		name    string
		backend string
		choice  any
	}{
		{name: "Messages tools disabled", backend: "messages", choice: "none"},
		{name: "Chat allowlist excludes search", backend: "chat_completions", choice: map[string]any{
			"type": "allowed_tools", "mode": "auto",
			"tools": []any{map[string]any{"type": "function", "name": "skill"}},
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := map[string]any{
				"input":             "do not search",
				"max_output_tokens": 4096,
				"tools":             []any{map[string]any{"type": "function", "name": "skill", "parameters": map[string]any{"type": "object"}}},
				"tool_choice":       test.choice,
			}
			body, _ := json.Marshal(root)
			request, err := adaptFacadeRequest(body, config.Route{
				APIBackend: test.backend, WireModel: "wire", Host: "relay.example", SupportsBackendSearch: true,
			}, wireResponses)
			if err != nil {
				t.Fatal(err)
			}
			if request.HostedWebSearch || request.ProxyAddedWebSearch {
				t.Fatalf("search was enabled against the structured choice: %#v", request)
			}
			wire, _ := decodeRequestObject(request.Body)
			if nativeRequestHasSearchForTest(wire, request.Protocol) {
				t.Fatalf("hosted search reached upstream: %#v", wire)
			}
		})
	}
}

func nativeRequestHasSearchForTest(root map[string]any, protocol wireProtocol) bool {
	switch protocol {
	case wireMessages:
		for _, raw := range anySlice(root["tools"]) {
			tool, _ := raw.(map[string]any)
			if isHostedWebSearchType(stringValue(tool["type"])) {
				return true
			}
		}
	case wireChatCompletions:
		return root["web_search_options"] != nil || root["search_parameters"] != nil
	case wireResponses:
		return hasHostedSearchTool(root)
	}
	return false
}

func TestMessagesHostedSearchResponseBlocksAreKeptAwayFromBuildConsumer(t *testing.T) {
	root := map[string]any{"content": []any{
		map[string]any{"type": "server_tool_use", "id": "srv_1", "name": "web_search", "input": map[string]any{"query": "q"}},
		map[string]any{"type": "web_search_tool_result", "tool_use_id": "srv_1", "content": []any{}},
		map[string]any{"type": "text", "text": "answer", "citations": []any{map[string]any{"url": "https://example.test"}}},
		map[string]any{"type": "tool_use", "id": "call_1", "name": "skill", "input": map[string]any{}},
	}}
	if !stripMessagesHostedSearchBlocks(root) {
		t.Fatal("hosted blocks were not removed")
	}
	content := anySlice(root["content"])
	if len(content) != 2 || stringValue(content[0].(map[string]any)["type"]) != "text" || stringValue(content[1].(map[string]any)["type"]) != "tool_use" {
		t.Fatalf("supported blocks changed: %#v", content)
	}
	if err := validateMessagesContentBlock(map[string]any{"type": "future_server_tool"}); err == nil {
		t.Fatal("strict cross-protocol validator accepted an unknown block")
	}
	if err := validateNativeMessagesContentBlock(map[string]any{"type": "future_server_tool", "future": true}); err != nil {
		t.Fatalf("native validator rejected a future provider extension: %v", err)
	}
	if err := validateNativeMessagesContentBlock(map[string]any{"type": "text"}); err == nil {
		t.Fatal("native validator stopped validating known block shapes")
	}

	filter := newMessagesHostedSearchStreamFilter(facadeRequest{Protocol: wireMessages, HostedWebSearch: true})
	if filter.keep(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "server_tool_use"}}) ||
		filter.keep(map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": "{}"}}) ||
		filter.keep(map[string]any{"type": "content_block_stop", "index": 1}) ||
		filter.keep(map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "citations_delta"}}) {
		t.Fatal("hosted-only Messages stream frame reached Grok Build")
	}
	if !filter.keep(map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "text", "text": ""}}) {
		t.Fatal("ordinary text frame was removed")
	}
}

func TestNativeClientToolDescriptionsAndAliasesWorkInEveryProtocol(t *testing.T) {
	tests := []struct {
		backend string
		body    string
	}{
		{"responses", `{"input":"hi","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}},{"type":"function","name":"web_fetch","parameters":{"type":"object"}}]}`},
		{"messages", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":100,"tools":[{"name":"web_search","input_schema":{"type":"object"}},{"name":"web_fetch","input_schema":{"type":"object"}}]}`},
		{"chat_completions", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object"}}},{"type":"function","function":{"name":"web_fetch","parameters":{"type":"object"}}}]}`},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			protocol := backendProtocol(t, test.backend)
			request, err := adaptFacadeRequest([]byte(test.body), config.Route{APIBackend: test.backend, WireModel: "wire"}, protocol)
			if err != nil {
				t.Fatal(err)
			}
			if request.ClientSearchAlias == "" {
				t.Fatal("client web_search was not isolated from provider-reserved names")
			}
			root, _ := decodeRequestObject(request.Body)
			tools := anySlice(root["tools"])
			if functionToolName(tools[0].(map[string]any)) != request.ClientSearchAlias {
				t.Fatalf("search tool was not aliased: %#v", root)
			}
			if !strings.Contains(toolDescription(tools[0].(map[string]any)), "Search the public web") ||
				!strings.Contains(toolDescription(tools[1].(map[string]any)), "Fetch and read one specific URL") {
				t.Fatalf("tool descriptions were not completed: %#v", root)
			}
		})
	}
}

func TestResponsesToolHistoryValidation(t *testing.T) {
	valid := []any{
		map[string]any{"type": "function_call", "call_id": "call_1"},
		map[string]any{"type": "function_call", "call_id": "call_2"},
		map[string]any{"type": "function_call_output", "call_id": "call_2"},
		map[string]any{"type": "function_call_output", "call_id": "call_1"},
	}
	if err := validateResponsesToolHistory(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		input []any
		want  string
	}{
		{"missing", []any{map[string]any{"type": "function_call", "call_id": "call_1"}}, "missing function_call_output"},
		{"orphan", []any{map[string]any{"type": "function_call_output", "call_id": "call_1"}}, "no preceding unresolved"},
		{"duplicate call", []any{map[string]any{"type": "function_call", "call_id": "call_1"}, map[string]any{"type": "function_call", "call_id": "call_1"}}, "duplicate function_call"},
		{"duplicate result", append(append([]any{}, valid...), map[string]any{"type": "function_call_output", "call_id": "call_1"}), "duplicate function_call_output"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateResponsesToolHistory(test.input); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func TestResponsesParallelToolsConvertToOneImmediateMessagesBatch(t *testing.T) {
	root := map[string]any{
		"model":             "wire",
		"max_output_tokens": 4096,
		"input": []any{
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "skill", "arguments": "{}"},
			map[string]any{"type": "function_call", "call_id": "call_2", "name": "skill", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_2", "output": "second"},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "first"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		},
	}
	converted, err := responsesToMessagesRequest(root)
	if err != nil {
		t.Fatal(err)
	}
	messages := anySlice(converted["messages"])
	if len(messages) != 3 {
		t.Fatalf("converted message count=%d body=%#v", len(messages), converted)
	}
	assistant := messages[0].(map[string]any)
	results := messages[1].(map[string]any)
	if assistant["role"] != "assistant" || len(contentBlocks(assistant["content"])) != 2 ||
		results["role"] != "user" || len(contentBlocks(results["content"])) != 2 {
		t.Fatalf("parallel calls/results were not grouped: %#v", messages)
	}
	if err := validateMessagesToolHistory(messages); err != nil {
		t.Fatalf("converted history is invalid: %v\n%#v", err, messages)
	}
}

func TestResponsesUserIsolationSurvivesProtocolConversion(t *testing.T) {
	root := map[string]any{
		"model":             "wire",
		"input":             "hello",
		"user":              "tenant_user-42",
		"max_output_tokens": 4096,
	}

	messages, err := responsesToMessagesRequest(root)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := messages["metadata"].(map[string]any)
	if metadata["user_id"] != "tenant_user-42" {
		t.Fatalf("Responses user was lost in Messages conversion: %#v", messages)
	}

	chat, err := responsesToChatRequest(root, config.Route{})
	if err != nil {
		t.Fatal(err)
	}
	if chat["user"] != "tenant_user-42" {
		t.Fatalf("Responses user was lost in Chat conversion: %#v", chat)
	}
}

func TestResponsesSearchHistoryMatchesGrokMessagesSummary(t *testing.T) {
	converted, err := responsesToMessagesRequest(map[string]any{
		"model": "wire", "max_output_tokens": 128,
		"input": []any{
			map[string]any{"role": "user", "content": "find current news"},
			map[string]any{
				"type": "web_search_call", "id": "ws_1", "status": "completed",
				"action": map[string]any{
					"type": "search", "query": "current news",
					"sources": []any{map[string]any{"type": "url", "url": "https://example.test"}},
				},
			},
			map[string]any{"role": "assistant", "content": "answer"},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages := anySlice(converted["messages"])
	if len(messages) != 3 {
		t.Fatalf("messages=%#v", messages)
	}
	assistant := messages[1].(map[string]any)
	blocks := contentBlocks(assistant["content"])
	if len(blocks) != 2 || blocks[0]["type"] != "text" ||
		blocks[0]["text"] != "[backend web_search] search: current news" ||
		blocks[1]["text"] != "answer" {
		t.Fatalf("search history did not match Grok Messages summary: %#v", messages)
	}
}

func TestMessagesBridgeAppliesStableGrokBuildCacheBreakpoints(t *testing.T) {
	root := map[string]any{
		"system": "stable system",
		"messages": []any{
			map[string]any{"role": "user", "content": "first question"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "private", "signature": "sig"},
				map[string]any{"type": "text", "text": "first answer"},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "done"},
				map[string]any{"type": "text", "text": "second question"},
			}},
		},
	}
	applyMessagesCacheBreakpoints(root)
	first, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if countJSONCacheBreakpoints(root) != 3 {
		t.Fatalf("cache breakpoints=%d body=%s", countJSONCacheBreakpoints(root), first)
	}
	applyMessagesCacheBreakpoints(root)
	second, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("cache breakpoint placement is not idempotent:\nfirst=%s\nsecond=%s", first, second)
	}
}

func countJSONCacheBreakpoints(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		count := 0
		if control, _ := typed["cache_control"].(map[string]any); stringValue(control["type"]) == "ephemeral" {
			count++
		}
		for key, child := range typed {
			if key != "cache_control" {
				count += countJSONCacheBreakpoints(child)
			}
		}
		return count
	case []any:
		count := 0
		for _, child := range typed {
			count += countJSONCacheBreakpoints(child)
		}
		return count
	default:
		return 0
	}
}

func TestMessagesParallelToolResultsMustBeOneImmediateUserBatch(t *testing.T) {
	valid := []any{
		map[string]any{"role": "user", "content": "read both"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "call_1", "name": "skill", "input": map[string]any{}},
			map[string]any{"type": "tool_use", "id": "call_2", "name": "skill", "input": map[string]any{}},
		}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "call_2", "content": "second"},
			map[string]any{"type": "tool_result", "tool_use_id": "call_1", "content": "first"},
			map[string]any{"type": "text", "text": "continue"},
		}},
	}
	if err := validateMessagesToolHistory(valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		messages []any
		want     string
	}{
		{"missing", valid[:2], "missing tool_result"},
		{"interrupted", append(append([]any{}, valid[:2]...), map[string]any{"role": "assistant", "content": "wrong"}), "immediately following user"},
		{"orphan", []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_1"}}}}, "no immediately preceding"},
		{"text before result", append(append([]any{}, valid[:2]...), map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "bad"}, map[string]any{"type": "tool_result", "tool_use_id": "call_1"}}}), "must precede other user content"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateMessagesToolHistory(test.messages); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func TestChatParallelToolResultsMustBeContiguous(t *testing.T) {
	valid := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function"},
			map[string]any{"id": "call_2", "type": "function"},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "second"},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "first"},
		map[string]any{"role": "user", "content": "continue"},
	}
	if err := validateChatToolHistory(valid); err != nil {
		t.Fatal(err)
	}
	if err := validateChatToolHistory(valid[:2]); err == nil || !strings.Contains(err.Error(), "missing tool result") {
		t.Fatalf("partial results error=%v", err)
	}
	interrupted := []any{valid[0], valid[1], map[string]any{"role": "user", "content": "too early"}}
	if err := validateChatToolHistory(interrupted); err == nil || !strings.Contains(err.Error(), "missing tool result") {
		t.Fatalf("interrupted results error=%v", err)
	}
}

func TestRouteHeadersNeverLeakLoginOAuth(t *testing.T) {
	header := http.Header{}
	header.Set("Authorization", "Bearer official-login-oauth")
	header.Set("X-Xai-Token-Auth", "official-xai-session")
	header.Set("X-Api-Key", "incoming-key")
	applyRouteHeaders(header, config.Route{APIKey: "channel-key", AuthScheme: "bearer"}, wireResponses, http.Header{
		"Authorization": []string{"Bearer official-login-oauth"},
	})
	if header.Get("Authorization") != "Bearer channel-key" || header.Get("X-Api-Key") != "" || header.Get("X-Xai-Token-Auth") != "official-xai-session" {
		t.Fatalf("channel auth was not isolated before safe-header copying: %#v", header)
	}
	dst := http.Header{}
	copySafeRequestHeaders(dst, header)
	if dst.Get("Authorization") != "" || dst.Get("X-Xai-Token-Auth") != "" {
		t.Fatalf("session auth escaped safe copy: %#v", dst)
	}
}

func TestSafeResponseHeadersDropHopByHopAndCookies(t *testing.T) {
	src := http.Header{
		"Connection":       []string{"X-Private-Hop"},
		"X-Private-Hop":    []string{"secret"},
		"Set-Cookie":       []string{"session=secret"},
		"Retry-After":      []string{"3"},
		"X-Should-Retry":   []string{"false"},
		"X-Upstream-Trace": []string{"trace"},
	}
	dst := http.Header{}
	copySafeResponseHeaders(dst, src)
	if dst.Get("Connection") != "" || dst.Get("X-Private-Hop") != "" || dst.Get("Set-Cookie") != "" {
		t.Fatalf("unsafe response headers survived: %#v", dst)
	}
	if dst.Get("Retry-After") != "3" || dst.Get("X-Should-Retry") != "false" || dst.Get("X-Upstream-Trace") != "trace" {
		t.Fatalf("safe retry/trace headers were lost: %#v", dst)
	}
}

func TestSafeUpstreamErrorRedactsURLSecrets(t *testing.T) {
	detail := safeUpstreamError(&url.Error{
		Op: "Post", URL: "https://api.example/tenant/secret-path-token/responses?api_key=secret-query-token",
		Err: errors.New("connection refused"),
	})
	if strings.Contains(detail, "secret-path-token") || strings.Contains(detail, "secret-query-token") ||
		!strings.Contains(detail, "https://api.example/.../responses") {
		t.Fatalf("unsafe diagnostic=%q", detail)
	}
}

func buildClientSearchBody(query string) []byte {
	return buildClientSearchBodyWithTool(query, map[string]any{"type": "web_search"})
}

func buildClientSearchBodyWithTool(query string, tool map[string]any) []byte {
	root := map[string]any{
		"model": "display", "input": query, "tools": []any{tool}, "store": false,
		"temperature": 0.1, "top_p": 0.95, "max_output_tokens": 8192,
	}
	data, _ := json.Marshal(root)
	return data
}

func backendProtocol(t *testing.T, backend string) wireProtocol {
	t.Helper()
	protocol, err := routeProtocol(config.Route{APIBackend: backend})
	if err != nil {
		t.Fatal(err)
	}
	return protocol
}

func summarizeSearchCounts(root map[string]any) (int, int) {
	data, _ := json.Marshal(root)
	_, _, hosted, _, x := summarizeBody(data)
	return hosted, x
}

func summarizeXSearch(root map[string]any) int {
	_, x := summarizeSearchCounts(root)
	return x
}

func toolDescription(tool map[string]any) string {
	if function, _ := tool["function"].(map[string]any); function != nil {
		return stringValue(function["description"])
	}
	return stringValue(tool["description"])
}
