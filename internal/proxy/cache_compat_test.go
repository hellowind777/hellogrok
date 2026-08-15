package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

type cacheProvider struct {
	name  string
	model string
	host  string
}

func cacheProviders() []cacheProvider {
	return []cacheProvider{
		{name: "gpt", model: "gpt-future-model", host: "api.openai.com"},
		{name: "claude", model: "claude-future-model", host: "api.anthropic.com"},
		{name: "grok", model: "grok-future-model", host: "api.x.ai"},
		{name: "gemini", model: "gemini-future-model", host: "generativelanguage.googleapis.com"},
		{name: "deepseek-current", model: "deepseek-v4-pro", host: "api.deepseek.com"},
		{name: "deepseek-future", model: "deepseek-future-model", host: "api.deepseek.com"},
		{name: "generic-relay", model: "future-provider-model", host: "relay.example"},
	}
}

func TestNativeProtocolCachePrefixStabilityMatrix(t *testing.T) {
	protocols := []struct {
		name     string
		backend  string
		protocol wireProtocol
		history  string
	}{
		{name: "responses", backend: "responses", protocol: wireResponses, history: "input"},
		{name: "messages", backend: "messages", protocol: wireMessages, history: "messages"},
		{name: "chat", backend: "chat_completions", protocol: wireChatCompletions, history: "messages"},
	}

	for _, model := range cacheProviders() {
		for _, protocol := range protocols {
			t.Run(model.name+"/"+protocol.name, func(t *testing.T) {
				route := config.Route{
					ChannelID: "cache", WireModel: model.model, Host: model.host,
					APIBackend: protocol.backend, APIBackendConfigured: true,
				}
				firstBody := nativeCacheRequest(t, protocol.protocol, false)
				secondBody := nativeCacheRequest(t, protocol.protocol, true)
				first := adaptCacheRequest(t, firstBody, route, protocol.protocol)
				second := adaptCacheRequest(t, secondBody, route, protocol.protocol)

				if first.Protocol != protocol.protocol || second.Protocol != protocol.protocol {
					t.Fatalf("native protocol changed: first=%s second=%s", first.Protocol, second.Protocol)
				}
				firstRoot := decodeCacheRequest(t, first.Body)
				secondRoot := decodeCacheRequest(t, second.Body)
				assertJSONPrefix(t, firstRoot[protocol.history], secondRoot[protocol.history])
				if !reflect.DeepEqual(firstRoot["tools"], secondRoot["tools"]) {
					t.Fatalf("tool definitions changed between turns:\nfirst=%s\nsecond=%s", first.Body, second.Body)
				}
				if !reflect.DeepEqual(firstRoot["vendor_cache_policy"], secondRoot["vendor_cache_policy"]) {
					t.Fatalf("unknown provider cache policy was not preserved: first=%#v second=%#v",
						firstRoot["vendor_cache_policy"], secondRoot["vendor_cache_policy"])
				}
				if protocol.protocol == wireResponses {
					for _, field := range []string{"prompt_cache_key", "prompt_cache_retention"} {
						if firstRoot[field] != secondRoot[field] || firstRoot[field] == nil {
							t.Fatalf("Responses %s changed: first=%v second=%v", field, firstRoot[field], secondRoot[field])
						}
					}
				}
				if protocol.protocol == wireMessages && countJSONCacheBreakpoints(firstRoot) < 2 {
					t.Fatalf("native Messages cache controls were lost: %s", first.Body)
				}
				if repeated := adaptCacheRequest(t, firstBody, route, protocol.protocol); !bytes.Equal(first.Body, repeated.Body) {
					t.Fatalf("identical request normalized differently:\nfirst=%s\nrepeat=%s", first.Body, repeated.Body)
				}
			})
		}
	}
}

func TestUnconfiguredBackendCachePrefixFollowsIncomingProtocolMatrix(t *testing.T) {
	protocols := []struct {
		name     string
		protocol wireProtocol
		history  string
	}{
		{name: "responses", protocol: wireResponses, history: "input"},
		{name: "messages", protocol: wireMessages, history: "messages"},
		{name: "chat", protocol: wireChatCompletions, history: "messages"},
	}

	for _, provider := range cacheProviders() {
		for _, protocol := range protocols {
			t.Run(provider.name+"/"+protocol.name, func(t *testing.T) {
				route := config.Route{
					ChannelID: "catalog-cache", WireModel: provider.model, Host: provider.host,
				}
				first := adaptCacheRequest(t, nativeCacheRequest(t, protocol.protocol, false), route, protocol.protocol)
				second := adaptCacheRequest(t, nativeCacheRequest(t, protocol.protocol, true), route, protocol.protocol)
				if first.Protocol != protocol.protocol || second.Protocol != protocol.protocol {
					t.Fatalf("catalog protocol was not followed: first=%s second=%s", first.Protocol, second.Protocol)
				}
				firstRoot := decodeCacheRequest(t, first.Body)
				secondRoot := decodeCacheRequest(t, second.Body)
				assertJSONPrefix(t, firstRoot[protocol.history], secondRoot[protocol.history])
			})
		}
	}
}

func TestResponsesBridgeCachePrefixStabilityMatrix(t *testing.T) {
	for _, provider := range cacheProviders() {
		for _, backend := range []string{"responses", "messages", "chat_completions"} {
			t.Run(provider.name+"/"+backend, func(t *testing.T) {
				dialect := config.ChatSearchDialect("")
				if backend == "chat_completions" {
					dialect = config.ChatSearchDialectWebSearchOptions
				}
				route := config.Route{
					ChannelID: "bridge", WireModel: provider.model, Host: provider.host,
					APIBackend: backend, APIBackendConfigured: true, SupportsBackendSearch: true,
					ChatSearchDialect: dialect,
				}
				firstBody := bridgeCacheRequest(t, false)
				secondBody := bridgeCacheRequest(t, true)
				thirdBody := bridgeCacheRequestAtTurn(t, 3)
				first := adaptCacheRequest(t, firstBody, route, wireResponses)
				second := adaptCacheRequest(t, secondBody, route, wireResponses)
				third := adaptCacheRequest(t, thirdBody, route, wireResponses)
				firstRoot := decodeCacheRequest(t, first.Body)
				secondRoot := decodeCacheRequest(t, second.Body)
				thirdRoot := decodeCacheRequest(t, third.Body)

				history := "messages"
				if first.Protocol == wireResponses {
					history = "input"
				}
				if first.Protocol == wireMessages {
					assertCacheContentPrefix(t, firstRoot[history], secondRoot[history])
					assertCacheContentPrefix(t, secondRoot[history], thirdRoot[history])
					assertMessagesCacheBoundaryContinuity(t, firstRoot[history], secondRoot[history])
					assertMessagesCacheBoundaryContinuity(t, secondRoot[history], thirdRoot[history])
				} else {
					assertJSONPrefix(t, firstRoot[history], secondRoot[history])
					assertJSONPrefix(t, secondRoot[history], thirdRoot[history])
				}
				if !reflect.DeepEqual(firstRoot["tools"], secondRoot["tools"]) ||
					!reflect.DeepEqual(secondRoot["tools"], thirdRoot["tools"]) {
					t.Fatalf("bridged tool definitions changed between turns:\nfirst=%s\nsecond=%s\nthird=%s", first.Body, second.Body, third.Body)
				}
				switch first.Protocol {
				case wireResponses:
					if firstRoot["prompt_cache_key"] != "conversation-stable" ||
						firstRoot["prompt_cache_retention"] != "24h" ||
						thirdRoot["prompt_cache_key"] != "conversation-stable" ||
						thirdRoot["prompt_cache_retention"] != "24h" {
						t.Fatalf("Responses cache controls were lost: first=%s third=%s", first.Body, third.Body)
					}
				case wireMessages:
					if countJSONCacheBreakpoints(firstRoot) != 2 || countJSONCacheBreakpoints(secondRoot) != 3 ||
						countJSONCacheBreakpoints(thirdRoot) != 3 {
						t.Fatalf("bridged Messages breakpoints first=%d second=%d third=%d\nfirst=%s\nsecond=%s\nthird=%s",
							countJSONCacheBreakpoints(firstRoot), countJSONCacheBreakpoints(secondRoot), countJSONCacheBreakpoints(thirdRoot),
							first.Body, second.Body, third.Body)
					}
					if !reflect.DeepEqual(firstRoot["system"], secondRoot["system"]) ||
						!reflect.DeepEqual(secondRoot["system"], thirdRoot["system"]) {
						t.Fatalf("Messages system cache prefix changed:\nfirst=%s\nsecond=%s\nthird=%s", first.Body, second.Body, third.Body)
					}
				case wireChatCompletions:
					options, _ := firstRoot["stream_options"].(map[string]any)
					if options["include_usage"] != true {
						t.Fatalf("Chat usage reporting was not requested: %s", first.Body)
					}
				}
				if repeated := adaptCacheRequest(t, firstBody, route, wireResponses); !bytes.Equal(first.Body, repeated.Body) {
					t.Fatalf("identical bridged request normalized differently:\nfirst=%s\nrepeat=%s", first.Body, repeated.Body)
				}
			})
		}
	}
}

func TestNativeToolHistoryCachePrefixStabilityMatrix(t *testing.T) {
	protocols := []struct {
		name     string
		backend  string
		protocol wireProtocol
		history  string
	}{
		{name: "responses", backend: "responses", protocol: wireResponses, history: "input"},
		{name: "messages", backend: "messages", protocol: wireMessages, history: "messages"},
		{name: "chat", backend: "chat_completions", protocol: wireChatCompletions, history: "messages"},
	}

	for _, model := range cacheProviders() {
		for _, protocol := range protocols {
			t.Run(model.name+"/"+protocol.name, func(t *testing.T) {
				route := config.Route{
					ChannelID: "tool-cache", WireModel: model.model, Host: model.host,
					APIBackend: protocol.backend, APIBackendConfigured: true,
				}
				first := adaptCacheRequest(t, nativeToolHistoryRequest(t, protocol.protocol, false), route, protocol.protocol)
				second := adaptCacheRequest(t, nativeToolHistoryRequest(t, protocol.protocol, true), route, protocol.protocol)
				firstRoot := decodeCacheRequest(t, first.Body)
				secondRoot := decodeCacheRequest(t, second.Body)

				assertJSONPrefix(t, firstRoot[protocol.history], secondRoot[protocol.history])
				if !reflect.DeepEqual(firstRoot["tools"], secondRoot["tools"]) {
					t.Fatalf("tool definitions changed after replay:\nfirst=%s\nsecond=%s", first.Body, second.Body)
				}
				assertReasoningToolHistory(t, firstRoot, protocol.protocol)
			})
		}
	}
}

func TestBridgedToolHistoryCachePrefixStabilityMatrix(t *testing.T) {
	backends := []struct {
		name    string
		backend string
		history string
		dialect config.ChatSearchDialect
	}{
		{name: "responses", backend: "responses", history: "input"},
		{name: "messages", backend: "messages", history: "messages"},
		{name: "chat", backend: "chat_completions", history: "messages", dialect: config.ChatSearchDialectWebSearchOptions},
	}

	for _, provider := range cacheProviders() {
		for _, backend := range backends {
			t.Run(provider.name+"/"+backend.name, func(t *testing.T) {
				route := config.Route{
					ChannelID: "tool-bridge", WireModel: provider.model, Host: provider.host,
					APIBackend: backend.backend, APIBackendConfigured: true,
					SupportsBackendSearch: true, ChatSearchDialect: backend.dialect,
				}
				first := adaptCacheRequest(t, responsesToolHistoryRequest(t, false), route, wireResponses)
				second := adaptCacheRequest(t, responsesToolHistoryRequest(t, true), route, wireResponses)
				third := adaptCacheRequest(t, responsesToolHistoryRequestAtTurn(t, 3), route, wireResponses)
				firstRoot := decodeCacheRequest(t, first.Body)
				secondRoot := decodeCacheRequest(t, second.Body)
				thirdRoot := decodeCacheRequest(t, third.Body)

				if first.Protocol == wireMessages {
					assertCacheContentPrefix(t, firstRoot[backend.history], secondRoot[backend.history])
					assertCacheContentPrefix(t, secondRoot[backend.history], thirdRoot[backend.history])
					assertMessagesCacheBoundaryContinuity(t, firstRoot[backend.history], secondRoot[backend.history])
					assertMessagesCacheBoundaryContinuity(t, secondRoot[backend.history], thirdRoot[backend.history])
				} else {
					assertJSONPrefix(t, firstRoot[backend.history], secondRoot[backend.history])
					assertJSONPrefix(t, secondRoot[backend.history], thirdRoot[backend.history])
				}
				if !reflect.DeepEqual(firstRoot["tools"], secondRoot["tools"]) ||
					!reflect.DeepEqual(secondRoot["tools"], thirdRoot["tools"]) {
					t.Fatalf("bridged tool definitions changed after replay:\nfirst=%s\nsecond=%s\nthird=%s", first.Body, second.Body, third.Body)
				}
				assertReasoningToolHistory(t, firstRoot, first.Protocol)
			})
		}
	}
}

func nativeCacheRequest(t *testing.T, protocol wireProtocol, secondTurn bool) []byte {
	t.Helper()
	vendorPolicy := map[string]any{"namespace": "stable", "ttl": "long"}
	switch protocol {
	case wireResponses:
		input := []any{map[string]any{"type": "message", "role": "user", "content": "first question"}}
		if secondTurn {
			input = append(input,
				map[string]any{"type": "message", "role": "assistant", "content": "first answer"},
				map[string]any{"type": "message", "role": "user", "content": "second question"},
			)
		}
		return marshalCacheRequest(t, map[string]any{
			"model": "display", "instructions": "stable system", "input": input, "stream": true,
			"prompt_cache_key": "conversation-stable", "prompt_cache_retention": "24h",
			"vendor_cache_policy": vendorPolicy, "tools": responsesCacheTools(), "tool_choice": "auto",
		})
	case wireMessages:
		messages := []any{map[string]any{"role": "user", "content": []any{cacheTextBlock("first question", true)}}}
		if secondTurn {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": []any{cacheTextBlock("first answer", false)}},
				map[string]any{"role": "user", "content": []any{cacheTextBlock("second question", true)}},
			)
		}
		return marshalCacheRequest(t, map[string]any{
			"model": "display", "system": []any{cacheTextBlock("stable system", true)},
			"messages": messages, "max_tokens": 512, "stream": true,
			"vendor_cache_policy": vendorPolicy, "tools": messagesCacheTools(), "tool_choice": map[string]any{"type": "auto"},
		})
	case wireChatCompletions:
		messages := []any{
			map[string]any{"role": "system", "content": "stable system"},
			map[string]any{"role": "user", "content": "first question"},
		}
		if secondTurn {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": "first answer"},
				map[string]any{"role": "user", "content": "second question"},
			)
		}
		return marshalCacheRequest(t, map[string]any{
			"model": "display", "messages": messages, "stream": true,
			"stream_options": map[string]any{"include_usage": true}, "prompt_cache_key": "conversation-stable",
			"vendor_cache_policy": vendorPolicy, "tools": chatCacheTools(), "tool_choice": "auto",
		})
	default:
		t.Fatalf("unsupported protocol %s", protocol)
		return nil
	}
}

func bridgeCacheRequest(t *testing.T, secondTurn bool) []byte {
	turn := 1
	if secondTurn {
		turn = 2
	}
	return bridgeCacheRequestAtTurn(t, turn)
}

func bridgeCacheRequestAtTurn(t *testing.T, turn int) []byte {
	t.Helper()
	input := []any{map[string]any{"type": "message", "role": "user", "content": "first question"}}
	if turn >= 2 {
		input = append(input,
			map[string]any{"type": "message", "role": "assistant", "content": "first answer"},
			map[string]any{"type": "message", "role": "user", "content": "second question"},
		)
	}
	if turn >= 3 {
		input = append(input,
			map[string]any{"type": "message", "role": "assistant", "content": "second answer"},
			map[string]any{"type": "message", "role": "user", "content": "third question"},
		)
	}
	return marshalCacheRequest(t, map[string]any{
		"model": "display", "instructions": "stable system", "input": input, "stream": true,
		"max_output_tokens": 512,
		"prompt_cache_key":  "conversation-stable", "prompt_cache_retention": "24h",
		"tools": []any{map[string]any{
			"type": "function", "name": "save", "description": "Save data",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}},
		}},
		"tool_choice": "auto",
	})
}

func nativeToolHistoryRequest(t *testing.T, protocol wireProtocol, secondTurn bool) []byte {
	t.Helper()
	switch protocol {
	case wireResponses:
		return responsesToolHistoryRequest(t, secondTurn)
	case wireMessages:
		messages := []any{
			map[string]any{"role": "user", "content": []any{cacheTextBlock("look up both values", false)}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan both lookups", "signature": "cache-signature"},
				map[string]any{"type": "tool_use", "id": "call_a", "name": "lookup", "input": map[string]any{"key": "a"}},
				map[string]any{"type": "tool_use", "id": "call_b", "name": "lookup", "input": map[string]any{"key": "b"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call_a", "content": "one"},
				map[string]any{"type": "tool_result", "tool_use_id": "call_b", "content": "two"},
			}},
			map[string]any{"role": "assistant", "content": []any{cacheTextBlock("a is one; b is two", false)}},
			map[string]any{"role": "user", "content": []any{cacheTextBlock("summarize", true)}},
		}
		if secondTurn {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": []any{cacheTextBlock("summary", false)}},
				map[string]any{"role": "user", "content": []any{cacheTextBlock("continue", true)}},
			)
		}
		return marshalCacheRequest(t, map[string]any{
			"model": "display", "system": []any{cacheTextBlock("stable system", true)},
			"messages": messages, "max_tokens": 512, "stream": true,
			"tools": messagesCacheTools(), "tool_choice": map[string]any{"type": "auto"},
		})
	case wireChatCompletions:
		messages := []any{
			map[string]any{"role": "system", "content": "stable system"},
			map[string]any{"role": "user", "content": "look up both values"},
			map[string]any{"role": "assistant", "content": nil, "reasoning_content": "plan both lookups", "tool_calls": []any{
				map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"key":"a"}`}},
				map[string]any{"id": "call_b", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"key":"b"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": "one"},
			map[string]any{"role": "tool", "tool_call_id": "call_b", "content": "two"},
			map[string]any{"role": "assistant", "content": "a is one; b is two"},
			map[string]any{"role": "user", "content": "summarize"},
		}
		if secondTurn {
			messages = append(messages,
				map[string]any{"role": "assistant", "content": "summary"},
				map[string]any{"role": "user", "content": "continue"},
			)
		}
		return marshalCacheRequest(t, map[string]any{
			"model": "display", "messages": messages, "stream": true,
			"stream_options": map[string]any{"include_usage": true},
			"tools":          chatCacheTools(), "tool_choice": "auto",
		})
	default:
		t.Fatalf("unsupported protocol %s", protocol)
		return nil
	}
}

func responsesToolHistoryRequest(t *testing.T, secondTurn bool) []byte {
	turn := 1
	if secondTurn {
		turn = 2
	}
	return responsesToolHistoryRequestAtTurn(t, turn)
}

func responsesToolHistoryRequestAtTurn(t *testing.T, turn int) []byte {
	t.Helper()
	input := []any{
		map[string]any{"type": "message", "role": "user", "content": "look up both values"},
		map[string]any{
			"type": "reasoning", "id": "reasoning_cache", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": "plan both lookups"}},
			"content": []any{map[string]any{"type": "reasoning_text", "text": "lookup a, then b"}},
		},
		map[string]any{"type": "function_call", "id": "fc_a", "call_id": "call_a", "name": "lookup", "arguments": `{"key":"a"}`, "status": "completed"},
		map[string]any{"type": "function_call", "id": "fc_b", "call_id": "call_b", "name": "lookup", "arguments": `{"key":"b"}`, "status": "completed"},
		map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "one"},
		map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "two"},
		map[string]any{"type": "message", "role": "assistant", "content": "a is one; b is two"},
		map[string]any{"type": "message", "role": "user", "content": "summarize"},
	}
	if turn >= 2 {
		input = append(input,
			map[string]any{"type": "message", "role": "assistant", "content": "summary"},
			map[string]any{"type": "message", "role": "user", "content": "continue"},
		)
	}
	if turn >= 3 {
		input = append(input,
			map[string]any{"type": "message", "role": "assistant", "content": "continued"},
			map[string]any{"type": "message", "role": "user", "content": "finish"},
		)
	}
	return marshalCacheRequest(t, map[string]any{
		"model": "display", "instructions": "stable system", "input": input, "stream": true,
		"max_output_tokens": 512, "prompt_cache_key": "conversation-stable", "prompt_cache_retention": "24h",
		"tools": responsesCacheTools(), "tool_choice": "auto",
	})
}

func responsesCacheTools() []any {
	return []any{
		map[string]any{"type": "function", "name": "lookup", "description": "Look up data", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "save", "description": "Save data", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "web_search", "parameters": map[string]any{"type": "object"}},
	}
}

func messagesCacheTools() []any {
	return []any{
		map[string]any{"name": "lookup", "description": "Look up data", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "save", "description": "Save data", "input_schema": map[string]any{"type": "object"}},
		map[string]any{"name": "web_search", "input_schema": map[string]any{"type": "object"}},
	}
}

func chatCacheTools() []any {
	return []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "Look up data", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "save", "description": "Save data", "parameters": map[string]any{"type": "object"}}},
		map[string]any{"type": "function", "function": map[string]any{"name": "web_search", "parameters": map[string]any{"type": "object"}}},
	}
}

func cacheTextBlock(text string, breakpoint bool) map[string]any {
	block := map[string]any{"type": "text", "text": text}
	if breakpoint {
		block["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return block
}

func adaptCacheRequest(t *testing.T, body []byte, route config.Route, incoming wireProtocol) facadeRequest {
	t.Helper()
	request, err := adaptFacadeRequest(body, route, incoming)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func decodeCacheRequest(t *testing.T, body []byte) map[string]any {
	t.Helper()
	root, err := decodeRequestObject(body)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func marshalCacheRequest(t *testing.T, root map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func assertJSONPrefix(t *testing.T, first, second any) {
	t.Helper()
	firstItems, ok := first.([]any)
	if !ok {
		t.Fatalf("first history is not an array: %#v", first)
	}
	secondItems, ok := second.([]any)
	if !ok || len(secondItems) < len(firstItems) {
		t.Fatalf("second history does not extend the first: first=%#v second=%#v", first, second)
	}
	if !reflect.DeepEqual(firstItems, secondItems[:len(firstItems)]) {
		t.Fatalf("cached history prefix changed:\nfirst=%#v\nsecond-prefix=%#v", firstItems, secondItems[:len(firstItems)])
	}
}

func assertCacheContentPrefix(t *testing.T, first, second any) {
	t.Helper()
	assertJSONPrefix(t, withoutCacheControls(first), withoutCacheControls(second))
}

func assertMessagesCacheBoundaryContinuity(t *testing.T, first, second any) {
	t.Helper()
	firstMarkers := cacheBreakpointMessageIndices(first)
	secondMarkers := cacheBreakpointMessageIndices(second)
	if len(firstMarkers) == 0 || len(secondMarkers) == 0 {
		t.Fatalf("Messages cache boundaries are missing: first=%v second=%v", firstMarkers, secondMarkers)
	}
	previousTip := firstMarkers[len(firstMarkers)-1]
	for _, marker := range secondMarkers {
		if marker == previousTip {
			return
		}
	}
	t.Fatalf("previous Messages tip boundary %d was not retained: first=%v second=%v", previousTip, firstMarkers, secondMarkers)
}

func cacheBreakpointMessageIndices(value any) []int {
	var indices []int
	for index, raw := range anySlice(value) {
		if countJSONCacheBreakpoints(raw) > 0 {
			indices = append(indices, index)
		}
	}
	return indices
}

func withoutCacheControls(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, child := range typed {
			if key != "cache_control" {
				clean[key] = withoutCacheControls(child)
			}
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, child := range typed {
			clean[index] = withoutCacheControls(child)
		}
		return clean
	default:
		return typed
	}
}

func assertReasoningToolHistory(t *testing.T, root map[string]any, protocol wireProtocol) {
	t.Helper()
	switch protocol {
	case wireResponses:
		items := anySlice(root["input"])
		wantTypes := []string{"message", "reasoning", "function_call", "function_call", "function_call_output", "function_call_output"}
		if len(items) < len(wantTypes) {
			t.Fatalf("Responses history is incomplete: %#v", items)
		}
		for index, want := range wantTypes {
			item, _ := items[index].(map[string]any)
			if got := stringValue(item["type"]); got != want {
				t.Fatalf("Responses item %d type=%q want=%q history=%#v", index, got, want, items)
			}
		}
		reasoning, _ := items[1].(map[string]any)
		if !strings.Contains(reasoningInputText(reasoning), "plan both lookups") {
			t.Fatalf("Responses reasoning text changed: %#v", reasoning)
		}
		assertResponseCallOrder(t, items[2:6])
	case wireMessages:
		messages := anySlice(root["messages"])
		var assistant, results map[string]any
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			blocks := anySlice(message["content"])
			if len(blocks) >= 3 && stringValue(blocks[0].(map[string]any)["type"]) == "thinking" {
				assistant = message
			}
			if len(blocks) >= 2 && stringValue(blocks[0].(map[string]any)["type"]) == "tool_result" {
				results = message
			}
		}
		if assistant == nil || results == nil {
			t.Fatalf("Messages reasoning/tool batch is incomplete: %#v", messages)
		}
		blocks := anySlice(assistant["content"])
		if stringValue(blocks[0].(map[string]any)["thinking"]) == "" ||
			stringValue(blocks[1].(map[string]any)["type"]) != "tool_use" ||
			stringValue(blocks[2].(map[string]any)["type"]) != "tool_use" ||
			stringValue(blocks[1].(map[string]any)["id"]) != "call_a" ||
			stringValue(blocks[2].(map[string]any)["id"]) != "call_b" {
			t.Fatalf("Messages thinking/tool order changed: %#v", blocks)
		}
		if _, marked := blocks[0].(map[string]any)["cache_control"]; marked {
			t.Fatalf("Messages thinking block received an invalid cache breakpoint: %#v", blocks[0])
		}
		resultBlocks := anySlice(results["content"])
		if stringValue(resultBlocks[0].(map[string]any)["tool_use_id"]) != "call_a" ||
			stringValue(resultBlocks[1].(map[string]any)["tool_use_id"]) != "call_b" {
			t.Fatalf("Messages tool-result order changed: %#v", resultBlocks)
		}
	case wireChatCompletions:
		messages := anySlice(root["messages"])
		assistantIndex := -1
		for index, raw := range messages {
			message, _ := raw.(map[string]any)
			if stringValue(message["role"]) == "assistant" && len(anySlice(message["tool_calls"])) == 2 {
				assistantIndex = index
				break
			}
		}
		if assistantIndex < 0 || assistantIndex+2 >= len(messages) {
			t.Fatalf("Chat reasoning/tool batch is incomplete: %#v", messages)
		}
		assistant, _ := messages[assistantIndex].(map[string]any)
		if !strings.Contains(stringValue(assistant["reasoning_content"]), "plan both lookups") {
			t.Fatalf("Chat reasoning_content changed: %#v", assistant)
		}
		calls := anySlice(assistant["tool_calls"])
		if stringValue(calls[0].(map[string]any)["id"]) != "call_a" ||
			stringValue(calls[1].(map[string]any)["id"]) != "call_b" {
			t.Fatalf("Chat tool-call order changed: %#v", calls)
		}
		firstResult, _ := messages[assistantIndex+1].(map[string]any)
		secondResult, _ := messages[assistantIndex+2].(map[string]any)
		if stringValue(firstResult["tool_call_id"]) != "call_a" || stringValue(secondResult["tool_call_id"]) != "call_b" {
			t.Fatalf("Chat tool-result order changed: first=%#v second=%#v", firstResult, secondResult)
		}
	default:
		t.Fatalf("unsupported protocol %s", protocol)
	}
}

func assertResponseCallOrder(t *testing.T, values []any) {
	t.Helper()
	if len(values) != 4 {
		t.Fatalf("Responses tool batch length=%d want=4", len(values))
	}
	firstCall, _ := values[0].(map[string]any)
	secondCall, _ := values[1].(map[string]any)
	firstResult, _ := values[2].(map[string]any)
	secondResult, _ := values[3].(map[string]any)
	if stringValue(firstCall["call_id"]) != "call_a" || stringValue(secondCall["call_id"]) != "call_b" ||
		stringValue(firstResult["call_id"]) != "call_a" || stringValue(secondResult["call_id"]) != "call_b" {
		t.Fatalf("Responses tool-call/result order changed: %#v", values)
	}
}
