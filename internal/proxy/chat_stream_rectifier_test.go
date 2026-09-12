package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestGrokBuildAccumulatorEmptyNameWipesWithoutRectifier(t *testing.T) {
	raw := []map[string]any{
		chatToolChunk(0, "call_1", "run_terminal_command", "", nil),
		chatToolChunk(0, "call_1", "", `{"command":"git status"}`, "tool_calls"),
	}
	acc := grokBuildAccumulateChatToolCalls(raw)
	if acc[0].Name != "" {
		t.Fatalf("expected last-write empty name to wipe the tool, got %#v", acc[0])
	}
}

func TestChatRectifierEmptyNameDoesNotWipeGrokAccumulator(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	var out []map[string]any
	ingest := func(root map[string]any) {
		frames, _ := rectifier.ingest(root)
		out = append(out, frames...)
	}
	ingest(chatToolChunk(0, "call_1", "run_terminal_command", "", nil))
	ingest(chatToolChunk(0, "call_1", "", `{"command":"git status","description":"check git"}`, nil))
	ingest(chatFinishChunk("tool_calls"))
	acc := grokBuildAccumulateChatToolCalls(out)
	got := acc[0]
	if got.Name != "run_terminal_command" {
		t.Fatalf("name wiped: %#v frames=%s", got, mustJSON(out))
	}
	if !jsonObjectComplete(got.Arguments) {
		t.Fatalf("arguments not a single JSON object: %q", got.Arguments)
	}
	obj := parseToolArguments(got.Arguments)
	if stringArg(obj, "command") != "git status" {
		t.Fatalf("command lost: %#v", obj)
	}
}

func TestChatRectifierDoesNotCanonIncompleteJSON(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	var out []map[string]any
	frames, notes := rectifier.ingest(chatToolChunk(0, "call_1", "run_terminal_command", "", nil))
	out = append(out, frames...)
	if len(frames) != 0 || len(notes) > 0 {
		t.Fatalf("name-only frame leaked or was rewritten frames=%d notes=%v", len(frames), notes)
	}
	frames, _ = rectifier.ingest(chatToolChunk(0, "", "", `{"command":"git`, nil))
	out = append(out, frames...)
	if len(frames) != 0 {
		t.Fatalf("incomplete argument frame leaked: %s", mustJSON(frames))
	}
	frames, notes = rectifier.ingest(chatToolChunk(0, "", "", ` status"}`, "tool_calls"))
	out = append(out, frames...)
	acc := grokBuildAccumulateChatToolCalls(out)
	got := acc[0]
	if got.Name != "run_terminal_command" {
		t.Fatalf("name=%q notes=%v", got.Name, notes)
	}
	if !jsonObjectComplete(got.Arguments) {
		t.Fatalf("assembled arguments are not one JSON object: %q", got.Arguments)
	}
	obj := parseToolArguments(got.Arguments)
	if stringArg(obj, "command") != "git status" {
		t.Fatalf("command=%#v args=%q", obj, got.Arguments)
	}
	if strings.Count(got.Arguments, `"description"`) > 1 {
		t.Fatalf("description concatenated into arguments: %q", got.Arguments)
	}
}

func TestChatRectifierResolvesBashAliasOnce(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	var out []map[string]any
	frames, _ := rectifier.ingest(chatToolChunk(0, "call_bash", "Bash", `{"command":"pwd"}`, nil))
	out = append(out, frames...)
	frames, notes := rectifier.ingest(chatFinishChunk("tool_calls"))
	out = append(out, frames...)
	acc := grokBuildAccumulateChatToolCalls(out)
	got := acc[0]
	if got.Name != "run_terminal_command" {
		t.Fatalf("alias not resolved: %#v notes=%v", got, notes)
	}
	obj := parseToolArguments(got.Arguments)
	if stringArg(obj, "command") != "pwd" || stringArg(obj, "description") == "" {
		t.Fatalf("args=%#v", obj)
	}
}

func TestChatRectifierPassesReasoningImmediately(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	frames, _ := rectifier.ingest(map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "Kimi-K3",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"reasoning_content": "checking git"},
		}},
	})
	if len(frames) != 1 {
		t.Fatalf("reasoning was buffered: %d", len(frames))
	}
	delta := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if stringValue(delta["reasoning_content"]) != "checking git" {
		t.Fatalf("reasoning lost: %#v", delta)
	}
}

func TestChatRectifierParallelToolsKeepIndexes(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	var out []map[string]any
	root := map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "GLM-5.3",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{
				map[string]any{"index": 0, "id": "call_a", "function": map[string]any{"name": "list_dir", "arguments": `{"target_directory":"."}`}},
				map[string]any{"index": 1, "id": "call_b", "function": map[string]any{"name": "LS", "arguments": `{"path":"src"}`}},
			}},
		}},
	}
	frames, _ := rectifier.ingest(root)
	out = append(out, frames...)
	frames, _ = rectifier.ingest(chatFinishChunk("tool_calls"))
	out = append(out, frames...)
	acc := grokBuildAccumulateChatToolCalls(out)
	if acc[0].Name != "list_dir" || acc[1].Name != "list_dir" {
		t.Fatalf("parallel names: %#v", acc)
	}
	if acc[0].ID == acc[1].ID {
		t.Fatalf("tool IDs collided: %#v", acc)
	}
}

func TestCanonicalizeSkipsIncompleteJSON(t *testing.T) {
	partial := `{"command":"git`
	got, changed := canonicalizeToolArguments("run_terminal_command", partial, grokBuildTools())
	if changed || got != partial {
		t.Fatalf("incomplete JSON was rewritten changed=%t got=%q", changed, got)
	}
	if jsonObjectComplete(partial) || jsonObjectComplete("") || jsonObjectComplete(`{"a":1}{"b":2}`) {
		t.Fatal("jsonObjectComplete accepted a non-object or concatenated payload")
	}
	if !jsonObjectComplete(`{"command":"pwd"}`) {
		t.Fatal("complete object rejected")
	}
}

func TestEnsureGLMChatToolStream(t *testing.T) {
	root := map[string]any{
		"model": "GLM-5.3",
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "list_dir"}}},
	}
	ensureGLMChatToolStream(root, config.Route{ChannelID: "GLM-5.3-ipix", WireModel: "GLM-5.3"}, wireChatCompletions)
	if root["tool_stream"] != true {
		t.Fatalf("tool_stream not set: %#v", root["tool_stream"])
	}
	root["tool_stream"] = false
	ensureGLMChatToolStream(root, config.Route{ChannelID: "GLM-5.3-ipix", WireModel: "GLM-5.3"}, wireChatCompletions)
	if root["tool_stream"] != false {
		t.Fatal("explicit tool_stream was overwritten")
	}
	kimi := map[string]any{
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "list_dir"}}},
	}
	ensureGLMChatToolStream(kimi, config.Route{ChannelID: "Kimi-K3-ipix", WireModel: "Kimi-K3"}, wireChatCompletions)
	if _, exists := kimi["tool_stream"]; exists {
		t.Fatal("Kimi request received GLM tool_stream")
	}
}

func TestNormalizeChatFinishReasonDropsEmptyAndMapsGLM(t *testing.T) {
	empty := map[string]any{"finish_reason": ""}
	normalizeChatFinishReason(empty)
	if _, present := empty["finish_reason"]; present {
		t.Fatalf("empty finish_reason kept: %#v", empty)
	}
	null := map[string]any{"finish_reason": nil}
	normalizeChatFinishReason(null)
	if _, present := null["finish_reason"]; present {
		t.Fatalf("null finish_reason kept: %#v", null)
	}
	glm := map[string]any{"finish_reason": "sensitive"}
	normalizeChatFinishReason(glm)
	if glm["finish_reason"] != "content_filter" {
		t.Fatalf("sensitive mapped to %#v", glm["finish_reason"])
	}
	unknown := map[string]any{"finish_reason": "not_a_real_reason"}
	normalizeChatFinishReason(unknown)
	if unknown["finish_reason"] != "stop" {
		t.Fatalf("unknown mapped to %#v", unknown["finish_reason"])
	}
}

func TestChatRectifierDropsEmptyFinishReasonOnContent(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	chunk := map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "GLM-5.3",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": "ok"},
			"finish_reason": "",
		}},
	}
	normalizeNativeChatRequiredFields(chunk, config.Route{WireModel: "GLM-5.3"}, true, "chatcmpl_1", 1)
	frames, _ := rectifier.ingest(chunk)
	if len(frames) != 1 {
		t.Fatalf("content frame dropped: %d", len(frames))
	}
	choice := frames[0]["choices"].([]any)[0].(map[string]any)
	if _, present := choice["finish_reason"]; present {
		t.Fatalf("empty finish_reason forwarded: %#v", choice)
	}
}

func TestChatRectifierToolFramesOmitFinishReason(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	_, _ = rectifier.ingest(chatToolChunk(0, "call_1", "run_terminal_command", `{"command":"pwd"}`, nil))
	frames, _ := rectifier.ingest(chatFinishChunk("tool_calls"))
	foundTool := false
	for _, frame := range frames {
		choice := frame["choices"].([]any)[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if len(anySlice(delta["tool_calls"])) == 0 {
			continue
		}
		foundTool = true
		if _, present := choice["finish_reason"]; present {
			t.Fatalf("tool delta carried finish_reason: %#v", choice)
		}
	}
	if !foundTool {
		t.Fatal("no tool frames")
	}
}

func TestChatRectifierForwardsNullUsageChunk(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	frames, _ := rectifier.ingest(map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "Kimi-K3",
		"choices": []any{},
		"usage":   nil,
	})
	if len(frames) != 1 {
		t.Fatalf("null usage chunk dropped: %d", len(frames))
	}
	if _, present := frames[0]["usage"]; !present {
		t.Fatalf("usage key missing: %#v", frames[0])
	}
}

func TestSanitizeChatReasoningDropsContinueProtocol(t *testing.T) {
	keep := "No output means clean tree, and no unpushed/unpulled commits."
	leak := ` previous task (checking git status) is complete. The user says "continue. If all tasks are complete, reply only: 任务已全部完成" — task already fully completed.`
	got := sanitizeChatReasoning(keep + leak)
	if got != keep {
		t.Fatalf("got %q", got)
	}
	if sanitizeChatReasoning(strings.TrimSpace(leak)) != "" {
		t.Fatalf("leak survived: %q", sanitizeChatReasoning(leak))
	}
	if sanitizeChatReasoning("checking git") != "checking git" {
		t.Fatal("legitimate reasoning was stripped")
	}
}

func TestChatRectifierDropsPostContentReasoning(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	var out []map[string]any
	ingest := func(root map[string]any) {
		frames, _ := rectifier.ingest(root)
		out = append(out, frames...)
	}
	ingest(chatDeltaChunk(map[string]any{"reasoning_content": "No output means clean tree, and no unpushed/unpulled commits."}))
	ingest(chatDeltaChunk(map[string]any{"content": "没有。工作区完全干净。"}))
	ingest(chatDeltaChunk(map[string]any{
		"reasoning_content": ` previous task (checking git status) is complete. The user says "continue. If all tasks are complete, reply only: 任务已全部完成" — task already fully completed.`,
	}))
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "No output means clean tree, and no unpushed/unpulled commits." {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "没有。工作区完全干净。" {
		t.Fatalf("text=%q", text)
	}
	for _, frame := range out {
		body := mustJSON([]map[string]any{frame})
		if strings.Contains(body, "任务已全部完成") || strings.Contains(body, "previous task") {
			t.Fatalf("protocol-meta leaked: %s", body)
		}
	}
}

func TestChatRectifierSplitsMixedReasoningBeforeContent(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{
		"reasoning_content": "checking git",
		"content":           "没有。",
	}))
	if len(frames) != 2 {
		t.Fatalf("expected reasoning then content, got %d: %s", len(frames), mustJSON(frames))
	}
	reasoning, text := grokBuildChatChannels(frames)
	if strings.Join(reasoning, "") != "checking git" {
		t.Fatalf("reasoning=%q", reasoning)
	}
	if strings.Join(text, "") != "没有。" {
		t.Fatalf("text=%q", text)
	}
	first := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if stringValue(first["content"]) != "" {
		t.Fatalf("reasoning prefix still carried content: %#v", first)
	}
	second := frames[1]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if stringValue(second["reasoning_content"]) != "" {
		t.Fatalf("content frame still carried reasoning: %#v", second)
	}
	for i, frame := range frames {
		if err := validateNativeSSEFrame(wireChatCompletions, frame); err != nil {
			t.Fatalf("frame %d invalid: %v body=%s", i, err, mustJSON(frame))
		}
	}
}

func TestChatRectifierStripsProtocolMetaFromPrefixThought(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{
		"reasoning_content": `No output means clean tree, and no unpushed/unpulled commits. previous task (checking git status) is complete. The user says "continue. If all tasks are complete, reply only: 任务已全部完成" — task already fully completed.`,
	}))
	if len(frames) != 1 {
		t.Fatalf("frames=%d %s", len(frames), mustJSON(frames))
	}
	delta := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	if stringValue(delta["reasoning_content"]) != "No output means clean tree, and no unpushed/unpulled commits." {
		t.Fatalf("reasoning=%q", delta["reasoning_content"])
	}
}

func TestCanonicalFromChatDropsProtocolMetaReasoning(t *testing.T) {
	body := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"没有。","reasoning_content":"No output means clean tree, and no unpushed/unpulled commits. previous task (checking git status) is complete. The user says \"continue. If all tasks are complete, reply only: 任务已全部完成\" — task already fully completed."},"finish_reason":"stop"}]}`)
	result, err := canonicalFromChat(body, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) < 2 {
		t.Fatalf("output=%s", mustJSON(result.Output))
	}
	reasoning := result.Output[0].(map[string]any)
	parts := anySlice(reasoning["content"])
	if len(parts) == 0 {
		t.Fatalf("missing reasoning content: %#v", reasoning)
	}
	text := stringValue(parts[0].(map[string]any)["text"])
	if text != "No output means clean tree, and no unpushed/unpulled commits." {
		t.Fatalf("reasoning text=%q", text)
	}
	if strings.Contains(text, "任务已全部完成") {
		t.Fatal("protocol-meta survived canonical conversion")
	}
}

func TestValidateRectifiedChatFrames(t *testing.T) {
	rectifier := newChatToolRectifier(grokBuildTools(), "")
	route := config.Route{ChannelID: "Kimi-K3-ipix", WireModel: "Kimi-K3"}
	chunk := chatToolChunk(0, "call_1", "run_terminal_command", `{"command":"git status"}`, nil)
	normalizeNativeChatRequiredFields(chunk, route, true, "chatcmpl_test", 1)
	frames, _ := rectifier.ingest(chunk)
	finish := chatFinishChunk("tool_calls")
	normalizeNativeChatRequiredFields(finish, route, true, "chatcmpl_test", 1)
	more, _ := rectifier.ingest(finish)
	frames = append(frames, more...)
	if len(frames) == 0 {
		t.Fatal("no frames")
	}
	for i, frame := range frames {
		if err := validateNativeSSEFrame(wireChatCompletions, frame); err != nil {
			t.Fatalf("frame %d invalid: %v body=%s", i, err, mustJSON(frame))
		}
	}
}

func chatDeltaChunk(delta map[string]any) map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "GLM-5.3",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": delta,
		}},
	}
}

func grokBuildChatChannels(frames []map[string]any) (reasoning, text []string) {
	for _, frame := range frames {
		for _, raw := range anySlice(frame["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			if t := firstString(delta, "reasoning_content", "reasoning"); t != "" {
				reasoning = append(reasoning, t)
			}
			if t := chatMessageText(delta["content"]); t != "" {
				text = append(text, t)
			}
		}
	}
	return reasoning, text
}

func chatToolChunk(index int, id, name, arguments string, finish any) map[string]any {
	function := map[string]any{"name": name, "arguments": arguments}
	call := map[string]any{"index": index, "function": function}
	if id != "" {
		call["id"] = id
		call["type"] = "function"
	}
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "Kimi-K3",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"tool_calls": []any{call}},
			"finish_reason": finish,
		}},
	}
}

func chatFinishChunk(reason string) map[string]any {
	return map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion.chunk", "created": 1, "model": "Kimi-K3",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
}

func grokBuildAccumulateChatToolCalls(frames []map[string]any) map[int]struct{ ID, Name, Arguments string } {
	acc := map[int]struct{ ID, Name, Arguments string }{}
	for _, frame := range frames {
		for _, raw := range anySlice(frame["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			for _, rawCall := range anySlice(delta["tool_calls"]) {
				call, _ := rawCall.(map[string]any)
				index := 0
				if value, present, valid := optionalCanonicalToken(call, "index"); present && valid {
					index = int(value)
				}
				entry := acc[index]
				if id := stringValue(call["id"]); id != "" {
					entry.ID = id
				}
				function, _ := call["function"].(map[string]any)
				if name, exists := function["name"]; exists && name != nil {
					entry.Name = stringValue(name)
				}
				if args, exists := function["arguments"]; exists && args != nil {
					entry.Arguments += encodeToolArguments(args)
				}
				acc[index] = entry
			}
		}
	}
	return acc
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
