package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestKimiChatHistoryDropsCrossTurnReasoning(t *testing.T) {
	route := kimiChatRoute()
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "把版本升到 3.8.7"},
		map[string]any{"role": "assistant", "content": "已提交", "reasoning_content": "bump 3.6 to 3.7", "thinking": "keep this out"},
		map[string]any{"role": "user", "content": "统一为v3.8.7"},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if stringValue(assistant["content"]) != "已提交" {
		t.Fatalf("content=%q", assistant["content"])
	}
	if _, ok := assistant["reasoning_content"]; ok {
		t.Fatalf("cross-turn kimi reasoning_content survived: %#v", assistant)
	}
	if _, ok := assistant["thinking"]; ok {
		t.Fatalf("cross-turn kimi thinking survived: %#v", assistant)
	}
}

func TestKimiChatHistoryKeepsIntraTurnToolReasoning(t *testing.T) {
	route := kimiChatRoute()
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "看 git 状态"},
		map[string]any{
			"role": "assistant", "content": nil,
			"reasoning_content": "need git status --porcelain",
			"tool_calls": []any{
				map[string]any{"id": "call_git", "type": "function", "function": map[string]any{
					"name": "run_terminal_command", "arguments": `{"command":"git status --porcelain"}`,
				}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_git", "content": ""},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if stringValue(assistant["reasoning_content"]) != "need git status --porcelain" {
		t.Fatalf("intra-turn kimi reasoning lost: %#v", assistant)
	}
}

func TestGLMChatHistoryDropsCrossTurnKeepsIntraTurn(t *testing.T) {
	route := config.Route{
		ChannelID:            "GLM-5.3-ipix",
		WireModel:            "GLM-5.3",
		Host:                 "ai.ipix.ink",
		APIBackend:           "chat_completions",
		APIBackendConfigured: true,
	}
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "先检查仓库"},
		map[string]any{"role": "assistant", "content": "干净", "reasoning_content": "previous task is complete"},
		map[string]any{"role": "user", "content": "继续改文件"},
		map[string]any{
			"role": "assistant", "content": nil,
			"reasoning_content": "I'll continue by reading the file",
			"tool_calls": []any{
				map[string]any{"id": "call_read", "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": `{"target_file":"plugin.json"}`,
				}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_read", "content": `{"version":"3.8.7"}`},
	})
	messages := anySlice(root["messages"])
	prior, _ := messages[1].(map[string]any)
	if _, ok := prior["reasoning_content"]; ok {
		t.Fatalf("cross-turn glm reasoning survived: %#v", prior)
	}
	current, _ := messages[3].(map[string]any)
	if stringValue(current["reasoning_content"]) != "I'll continue by reading the file" {
		t.Fatalf("intra-turn glm reasoning lost: %#v", current)
	}
}

func TestChatHistoryDoesNotInjectToolCallReasoningPlaceholder(t *testing.T) {
	route := kimiChatRoute()
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "列出目录"},
		map[string]any{
			"role": "assistant", "content": nil,
			"tool_calls": []any{
				map[string]any{"id": "call_ls", "type": "function", "function": map[string]any{
					"name": "list_dir", "arguments": `{"target_directory":"."}`,
				}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_ls", "content": "a.go"},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; ok {
		t.Fatalf("injected reasoning placeholder: %#v", assistant)
	}
}

func TestChatHistoryKeepsSignedThinkingAcrossTurns(t *testing.T) {
	route := kimiChatRoute()
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "first"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking", "thinking": "opaque plan", "signature": "sig-1"},
			map[string]any{"type": "text", "text": "done"},
		}, "reasoning_content": "plaintext leak", "reasoning_details": []any{
			map[string]any{"type": "reasoning.encrypted", "data": "blob"},
			map[string]any{"type": "reasoning.text", "text": "drop me"},
		}},
		map[string]any{"role": "user", "content": "second"},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if _, ok := assistant["reasoning_content"]; ok {
		t.Fatalf("plaintext reasoning_content survived: %#v", assistant)
	}
	details := anySlice(assistant["reasoning_details"])
	if len(details) != 1 {
		t.Fatalf("encrypted reasoning_details lost: %#v", assistant["reasoning_details"])
	}
	if stringValue(details[0].(map[string]any)["type"]) != "reasoning.encrypted" {
		t.Fatalf("wrong reasoning_details kept: %#v", details)
	}
	blocks := anySlice(assistant["content"])
	if len(blocks) != 2 {
		t.Fatalf("signed thinking block dropped: %#v", blocks)
	}
	if stringValue(blocks[0].(map[string]any)["signature"]) != "sig-1" || stringValue(blocks[1].(map[string]any)["text"]) != "done" {
		t.Fatalf("signed thinking rewrite: %#v", blocks)
	}
}

func TestDeepSeekChatHistoryKeepsCrossTurnReasoning(t *testing.T) {
	route := config.Route{
		ChannelID:            "deepseek-v4-flash-opencodego",
		WireModel:            "deepseek-chat",
		Host:                 "api.deepseek.com",
		OriginBase:           "https://api.deepseek.com",
		APIBackend:           "chat_completions",
		APIBackendConfigured: true,
	}
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "ok", "reasoning_content": "plan the lookup"},
		map[string]any{"role": "user", "content": "continue"},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if stringValue(assistant["reasoning_content"]) != "plan the lookup" {
		t.Fatalf("deepseek cross-turn reasoning replay lost: %#v", assistant)
	}
}

func TestMiMoChatHistoryKeepsCrossTurnReasoning(t *testing.T) {
	route := config.Route{
		ChannelID:            "mimo-pro",
		WireModel:            "mimo-v2.5-pro",
		Host:                 "api.xiaomimimo.com",
		APIBackend:           "chat_completions",
		APIBackendConfigured: true,
	}
	root := adaptChatMessages(t, route, []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "ok", "reasoning_content": "keep me"},
		map[string]any{"role": "user", "content": "next"},
	})
	assistant, _ := anySlice(root["messages"])[1].(map[string]any)
	if stringValue(assistant["reasoning_content"]) != "keep me" {
		t.Fatalf("mimo cross-turn reasoning replay lost: %#v", assistant)
	}
}

func kimiChatRoute() config.Route {
	return config.Route{
		ChannelID:            "Kimi-K3-ipix",
		WireModel:            "Kimi-K3",
		Host:                 "ai.ipix.ink",
		APIBackend:           "chat_completions",
		APIBackendConfigured: true,
	}
}

func chatTestTool(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "parameters": map[string]any{"type": "object"},
	}}
}

func adaptChatMessages(t *testing.T, route config.Route, messages []any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model":    route.WireModel,
		"stream":   true,
		"messages": messages,
		"tools": []any{
			chatTestTool("run_terminal_command"),
			chatTestTool("read_file"),
			chatTestTool("list_dir"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := adaptFacadeRequest(body, route, wireChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeRequestObject(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLiftChatWireFlattensContentArrayForGrokBuild(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{
		"content": []any{
			map[string]any{"type": "thinking", "thinking": "plan the lookup"},
			map[string]any{"type": "text", "text": "没有变更。"},
		},
	}))
	if len(frames) != 2 {
		t.Fatalf("expected split, got %d %s", len(frames), mustJSON(frames))
	}
	reasoning, text := grokBuildChatChannels(frames)
	if strings.Join(reasoning, "") != "plan the lookup" || strings.Join(text, "") != "没有变更。" {
		t.Fatalf("reasoning=%q text=%q", reasoning, text)
	}
	for i, frame := range frames {
		if err := validateNativeSSEFrame(wireChatCompletions, frame); err != nil {
			t.Fatalf("frame %d invalid for grok-build: %v body=%s", i, err, mustJSON(frame))
		}
	}
}

func TestLiftChatWireOpenRouterReasoningDetails(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{
		"reasoning": "inspect git",
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.text", "text": "inspect git"},
			map[string]any{"type": "reasoning.encrypted", "data": "abc"},
		},
	}))
	if len(frames) != 1 {
		t.Fatalf("frames=%d %s", len(frames), mustJSON(frames))
	}
	delta := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if stringValue(delta["reasoning_content"]) != "inspect git" {
		t.Fatalf("reasoning=%q", delta["reasoning_content"])
	}
	if _, exists := delta["reasoning_details"]; exists {
		t.Fatalf("reasoning_details leaked: %#v", delta)
	}
	if _, exists := delta["reasoning"]; exists {
		t.Fatalf("reasoning alias leaked: %#v", delta)
	}
	if err := validateNativeSSEFrame(wireChatCompletions, frames[0]); err != nil {
		t.Fatal(err)
	}
}

func TestLiftChatWireGeminiFunctionCall(t *testing.T) {
	delta := map[string]any{
		"functionCall": map[string]any{
			"name": "list_dir",
			"args": map[string]any{"target_directory": "."},
		},
	}
	liftChatWireDialect(delta)
	calls := anySlice(delta["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("functionCall not lifted: %#v", delta)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if stringValue(function["name"]) != "list_dir" {
		t.Fatalf("name=%#v", function)
	}
	if _, ok := function["arguments"].(string); !ok {
		t.Fatalf("arguments not string: %#v", function)
	}
	if _, exists := delta["functionCall"]; exists {
		t.Fatalf("functionCall leaked: %#v", delta)
	}
}

func TestLiftChatWireSingleToolCallObject(t *testing.T) {
	delta := map[string]any{
		"tool_calls": map[string]any{
			"id": "call_1", "name": "Read",
			"arguments": map[string]any{"target_file": "a.go"},
		},
	}
	liftChatWireDialect(delta)
	calls := anySlice(delta["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("object tool_calls not wrapped: %#v", delta)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if stringValue(function["name"]) != "Read" {
		t.Fatalf("name=%#v", function)
	}
	if _, ok := function["arguments"].(string); !ok {
		t.Fatalf("arguments not string: %#v", function)
	}
}

func TestLiftChatWireDropsPostContentArrayThinking(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"thinking": "tree is clean"})
	ingest(map[string]any{"content": []any{map[string]any{"type": "text", "text": "没有变更。"}}})
	ingest(map[string]any{
		"reasoning_details": []any{
			map[string]any{"type": "reasoning.text", "text": "If all tasks are complete, reply only: DONE."},
		},
	})
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "tree is clean" {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "没有变更。" {
		t.Fatalf("text=%q", text)
	}
	if strings.Contains(mustJSON(out), "reply only") {
		t.Fatalf("post-content thinking leaked: %s", mustJSON(out))
	}
}

func TestCanonicalFromChatContentPartsAndRefusal(t *testing.T) {
	body := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":"plan it. If all tasks are complete, reply only: DONE."},{"type":"text","text":"ok"}],"refusal":null},"finish_reason":"stop"}]}`)
	result, err := canonicalFromChat(body, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) < 2 {
		t.Fatalf("output=%s", mustJSON(result.Output))
	}
	text := stringValue(anySlice(result.Output[0].(map[string]any)["content"])[0].(map[string]any)["text"])
	if text != "plan it." {
		t.Fatalf("thinking parts=%q", text)
	}
	message := result.Output[1].(map[string]any)
	got := stringValue(anySlice(message["content"])[0].(map[string]any)["text"])
	if got != "ok" {
		t.Fatalf("text=%q", got)
	}
}

func TestChatStreamStateLiftsVendorThinkingBeforeText(t *testing.T) {
	delta := map[string]any{
		"thinking": "plan",
		"content": []any{
			map[string]any{"type": "text", "text": "ok"},
		},
	}
	liftChatWireDialect(delta)
	if stringValue(delta["reasoning_content"]) != "plan" {
		t.Fatalf("thinking not lifted: %#v", delta)
	}
	if stringValue(delta["content"]) != "ok" {
		t.Fatalf("content not flattened: %#v", delta)
	}
}
