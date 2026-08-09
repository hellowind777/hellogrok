package proxy

import (
	"encoding/json"
	"testing"
)

func TestClientSearchWireAliasAvoidsCollisionsAndLeavesHostedToolsUntouched(t *testing.T) {
	root := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "name": "web_search"},
			map[string]any{"type": "function", "name": clientWebSearchWireAliasBase},
			map[string]any{"type": "web_search", "name": "web_search"},
		},
		"tool_choice": map[string]any{"type": "function", "name": "web_search"},
		"input": []any{
			map[string]any{"type": "function_call", "name": "web_search", "call_id": "call_1", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": map[string]any{
				"type": "function_call", "name": "web_search",
			}},
		},
	}
	alias := chooseClientWebSearchWireAlias(root)
	if alias != clientWebSearchWireAliasBase+"_2" {
		t.Fatalf("collision-safe alias=%q", alias)
	}
	if !aliasClientWebSearchOnWire(root, alias, wireResponses) {
		t.Fatal("client search was not aliased")
	}
	tools := anySlice(root["tools"])
	client, _ := tools[0].(map[string]any)
	hosted, _ := tools[2].(map[string]any)
	choice, _ := root["tool_choice"].(map[string]any)
	call := anySlice(root["input"])[0].(map[string]any)
	if stringValue(client["name"]) != alias || stringValue(choice["name"]) != alias || stringValue(call["name"]) != alias {
		t.Fatalf("client names were not aliased: %#v %#v", client, choice)
	}
	if stringValue(hosted["name"]) != "web_search" {
		t.Fatalf("hosted tool was rewritten: %#v", hosted)
	}
	result := anySlice(root["input"])[1].(map[string]any)["output"].(map[string]any)
	if got := stringValue(result["name"]); got != "web_search" {
		t.Fatalf("tool result business payload was rewritten: %q", got)
	}
}

func TestClientSearchAliasRestoresOnlyProtocolResponseFields(t *testing.T) {
	alias := clientWebSearchWireAliasBase
	tests := []struct {
		name     string
		protocol wireProtocol
		root     map[string]any
		outer    func(map[string]any) map[string]any
		payload  func(map[string]any) map[string]any
	}{
		{
			name:     "responses",
			protocol: wireResponses,
			root: map[string]any{"output": []any{map[string]any{
				"type": "function_call", "name": alias,
				"arguments": `{"type":"function_call","name":"hellogrok_web_search"}`,
			}}},
			outer: func(root map[string]any) map[string]any { return anySlice(root["output"])[0].(map[string]any) },
			payload: func(root map[string]any) map[string]any {
				return map[string]any{"name": stringValue(anySlice(root["output"])[0].(map[string]any)["arguments"])}
			},
		},
		{
			name:     "messages",
			protocol: wireMessages,
			root: map[string]any{"content": []any{map[string]any{
				"type": "tool_use", "name": alias,
				"input": map[string]any{"type": "function_call", "name": alias},
			}}},
			outer: func(root map[string]any) map[string]any { return anySlice(root["content"])[0].(map[string]any) },
			payload: func(root map[string]any) map[string]any {
				return anySlice(root["content"])[0].(map[string]any)["input"].(map[string]any)
			},
		},
		{
			name:     "chat_completions",
			protocol: wireChatCompletions,
			root: map[string]any{"choices": []any{map[string]any{"message": map[string]any{
				"tool_calls": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": alias, "arguments": `{"type":"function_call","name":"hellogrok_web_search"}`,
				}}},
			}}}},
			outer: func(root map[string]any) map[string]any {
				message := anySlice(root["choices"])[0].(map[string]any)["message"].(map[string]any)
				return anySlice(message["tool_calls"])[0].(map[string]any)["function"].(map[string]any)
			},
			payload: func(root map[string]any) map[string]any {
				message := anySlice(root["choices"])[0].(map[string]any)["message"].(map[string]any)
				function := anySlice(message["tool_calls"])[0].(map[string]any)["function"].(map[string]any)
				return map[string]any{"name": stringValue(function["arguments"])}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payloadBefore := stringValue(test.payload(test.root)["name"])
			if !restoreClientWebSearchAlias(test.root, alias, test.protocol) {
				t.Fatal("structured response name was not restored")
			}
			if got := stringValue(test.outer(test.root)["name"]); got != "web_search" {
				t.Fatalf("restored name=%q", got)
			}
			if got := stringValue(test.payload(test.root)["name"]); got != payloadBefore {
				t.Fatalf("business payload changed: before=%q after=%q", payloadBefore, got)
			}
		})
	}
}

func TestClientSearchAliasRestoresNativeStreamToolNamesWithoutTouchingArguments(t *testing.T) {
	alias := clientWebSearchWireAliasBase
	tests := []struct {
		name      string
		protocol  wireProtocol
		root      map[string]any
		tool      func(map[string]any) map[string]any
		arguments func(map[string]any) any
	}{
		{
			name:     "responses output item added",
			protocol: wireResponses,
			root: map[string]any{
				"type": "response.output_item.added",
				"item": map[string]any{
					"type": "function_call", "name": alias,
					"arguments": `{"name":"hellogrok_web_search"}`,
				},
			},
			tool: func(root map[string]any) map[string]any {
				return root["item"].(map[string]any)
			},
			arguments: func(root map[string]any) any {
				return root["item"].(map[string]any)["arguments"]
			},
		},
		{
			name:     "responses output item done",
			protocol: wireResponses,
			root: map[string]any{
				"type": "response.output_item.done",
				"item": map[string]any{
					"type": "function_call", "name": alias,
					"arguments": `{"name":"hellogrok_web_search"}`,
				},
			},
			tool: func(root map[string]any) map[string]any {
				return root["item"].(map[string]any)
			},
			arguments: func(root map[string]any) any {
				return root["item"].(map[string]any)["arguments"]
			},
		},
		{
			name:     "messages content block start",
			protocol: wireMessages,
			root: map[string]any{
				"type": "content_block_start",
				"content_block": map[string]any{
					"type": "tool_use", "name": alias,
					"input": map[string]any{"name": alias},
				},
			},
			tool: func(root map[string]any) map[string]any {
				return root["content_block"].(map[string]any)
			},
			arguments: func(root map[string]any) any {
				return root["content_block"].(map[string]any)["input"]
			},
		},
		{
			name:     "chat completions delta",
			protocol: wireChatCompletions,
			root: map[string]any{
				"choices": []any{map[string]any{
					"delta": map[string]any{
						"tool_calls": []any{map[string]any{
							"type": "function",
							"function": map[string]any{
								"name": alias, "arguments": `{"name":"hellogrok_web_search"}`,
							},
						}},
					},
				}},
			},
			tool: func(root map[string]any) map[string]any {
				choice := anySlice(root["choices"])[0].(map[string]any)
				delta := choice["delta"].(map[string]any)
				call := anySlice(delta["tool_calls"])[0].(map[string]any)
				return call["function"].(map[string]any)
			},
			arguments: func(root map[string]any) any {
				choice := anySlice(root["choices"])[0].(map[string]any)
				delta := choice["delta"].(map[string]any)
				call := anySlice(delta["tool_calls"])[0].(map[string]any)
				return call["function"].(map[string]any)["arguments"]
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			argumentsBefore, err := json.Marshal(test.arguments(test.root))
			if err != nil {
				t.Fatal(err)
			}
			if !restoreClientWebSearchAlias(test.root, alias, test.protocol) {
				t.Fatal("stream tool name was not restored")
			}
			if got := stringValue(test.tool(test.root)["name"]); got != "web_search" {
				t.Fatalf("restored name=%q", got)
			}
			argumentsAfter, err := json.Marshal(test.arguments(test.root))
			if err != nil {
				t.Fatal(err)
			}
			if string(argumentsAfter) != string(argumentsBefore) {
				t.Fatalf("stream tool arguments changed: before=%s after=%s", argumentsBefore, argumentsAfter)
			}
		})
	}
}
