package proxy

import (
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

func TestBuildClientSearchFingerprintRejectsNearMatches(t *testing.T) {
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
		{"missing temperature", func(root map[string]any) { delete(root, "temperature") }},
		{"wrong temperature", func(root map[string]any) { root["temperature"] = 1 }},
		{"wrong top p", func(root map[string]any) { root["top_p"] = 1 }},
		{"wrong max output", func(root map[string]any) { root["max_output_tokens"] = 4096 }},
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
	if parameters["mode"] != "on" || len(sources) != 1 ||
		len(anySlice(sources[0].(map[string]any)["allowed_websites"])) != 2 {
		t.Fatalf("xAI search dialect=%#v", xaiRoot)
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
	if got := chatSearchDialect(config.Route{Host: "api.deepseek.com"}); got != config.ChatSearchDialectMessages {
		t.Fatalf("official DeepSeek host default=%q", got)
	}
	explicit := config.Route{Host: "api.deepseek.com", ChatSearchDialect: config.ChatSearchDialectWebSearchOptions}
	if got := chatSearchDialect(explicit); got != config.ChatSearchDialectWebSearchOptions {
		t.Fatalf("explicit dialect was ignored: %q", got)
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
}

func TestCapableMessagesConsumesResponsesAndUsesHostedSearch(t *testing.T) {
	body := []byte(`{
		"model":"display",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"search current news"}]}],
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
				"input":       "do not search",
				"tools":       []any{map[string]any{"type": "function", "name": "skill", "parameters": map[string]any{"type": "object"}}},
				"tool_choice": test.choice,
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
		t.Fatal("unknown block would still reach Grok Build's closed enum")
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
		"model": "wire",
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

func TestResponsesSearchHistoryRebuildsAdjacentMessagesBlocks(t *testing.T) {
	blocks := responseWebSearchToMessagesBlocks(map[string]any{
		"type": "web_search_call", "id": "ws_1",
		"action": map[string]any{
			"type": "search", "query": "current news",
			"sources": []any{map[string]any{"type": "url", "url": "https://example.test", "title": "Example"}},
		},
	})
	if len(blocks) != 2 || blocks[0]["type"] != "server_tool_use" ||
		blocks[1]["type"] != "web_search_tool_result" || blocks[1]["tool_use_id"] != "ws_1" {
		t.Fatalf("search history blocks=%#v", blocks)
	}
	results := anySlice(blocks[1]["content"])
	if len(results) != 1 || results[0].(map[string]any)["url"] != "https://example.test" {
		t.Fatalf("search sources were lost: %#v", blocks)
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
