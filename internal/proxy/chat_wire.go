package proxy

import (
	"strings"

	"github.com/hellowind777/hellogrok/internal/config"
)

// liftChatWireDialect rewrites one Chat Completions assistant object (delta or
// message) into the shape Grok Build's sampler actually deserializes:
// content and reasoning_content as strings, tool_calls as OpenAI function
// objects. Vendor keys are dropped so they cannot leak past the thought gate.
//
// This is the Grok Build counterpart of CC Switch's OpenAI→Anthropic
// converter: the client speaks one native protocol; the proxy absorbs every
// upstream dialect.
func liftChatWireDialect(obj map[string]any) {
	if obj == nil {
		return
	}
	content, contentThought, contentCalls := splitChatContent(obj["content"])
	wrapToolCallsArray(obj)
	liftChatMessageToolCalls(obj)
	if len(anySlice(obj["tool_calls"])) == 0 && len(contentCalls) > 0 {
		for _, raw := range contentCalls {
			call, _ := raw.(map[string]any)
			liftChatToolCallObject(call)
			if stringValue(call["id"]) == "" {
				call["id"] = compatID("call")
			}
		}
		obj["tool_calls"] = contentCalls
	}
	liftGeminiFunctionCall(obj)
	flattenChatContent(obj, content)
	if strings.TrimSpace(chatMessageText(obj["content"])) == "" {
		if refusal := firstString(obj, "refusal"); refusal != "" {
			obj["content"] = refusal
		}
	}
	delete(obj, "refusal")
	liftChatThoughtFields(obj)
	if contentThought != "" {
		writeChatReasoning(obj, appendUniqueThought(stringValue(obj["reasoning_content"]), contentThought))
	}
	peelThinkFromChatContent(obj)
}

func flattenChatContent(obj map[string]any, text string) {
	if obj == nil {
		return
	}
	raw, present := obj["content"]
	if !present {
		if text != "" {
			obj["content"] = text
		}
		return
	}
	if raw == nil {
		if text != "" {
			obj["content"] = text
		}
		return
	}
	if _, ok := raw.(string); ok {
		return
	}
	obj["content"] = text
}

func splitChatContent(value any) (text, thought string, calls []any) {
	if value == nil {
		return "", "", nil
	}
	if s, ok := value.(string); ok {
		return s, "", nil
	}
	var textParts, thoughtParts []string
	for _, raw := range anySlice(value) {
		switch part := raw.(type) {
		case string:
			if part != "" {
				textParts = append(textParts, part)
			}
		case map[string]any:
			typ := strings.ToLower(strings.TrimSpace(stringValue(part["type"])))
			switch typ {
			case "thinking", "reasoning", "reasoning_content", "thought", "reasoning_text":
				piece := thoughtValueText(part)
				if piece == "" {
					piece = firstString(part, "thinking", "text", "content")
				}
				if strings.TrimSpace(piece) != "" {
					thoughtParts = append(thoughtParts, piece)
				}
			case "refusal":
				if piece := firstString(part, "refusal", "text", "content"); piece != "" {
					textParts = append(textParts, piece)
				}
			case "image_url", "image", "input_audio", "audio", "file":
				continue
			case "tool_call", "function_call", "functioncall", "function":
				calls = append(calls, part)
			case "tool_use":
				calls = append(calls, map[string]any{
					"id":   firstString(part, "id"),
					"type": "function",
					"function": map[string]any{
						"name":      firstString(part, "name"),
						"arguments": encodeToolArguments(valueOr(part["input"], valueOr(part["arguments"], map[string]any{}))),
					},
				})
			default:
				if piece := firstString(part, "text", "content"); piece != "" {
					textParts = append(textParts, piece)
				}
			}
		}
	}
	return strings.Join(textParts, ""), strings.Join(thoughtParts, ""), calls
}

// replayChatReasoningHistory is true only for vendors whose Chat gateway
// requires every previous-turn reasoning_content, including across user
// turns. DeepSeek (with tools) and MiMo 400 without it. Everyone else keeps
// intra-turn CoT and drops cross-turn plaintext; see stripChatHistoryReasoning.
func replayChatReasoningHistory(route config.Route) bool {
	blob := strings.ToLower(strings.Join([]string{route.ChannelID, route.WireModel, route.Host, route.OriginBase}, " "))
	for _, needle := range []string{"deepseek", "mimo", "xiaomimimo"} {
		if strings.Contains(blob, needle) {
			return true
		}
	}
	return isOfficialDeepSeekRoute(route)
}

// stripChatHistoryReasoning drops plaintext CoT from assistant messages that
// sit before the latest user turn. Intra-turn tool loops (assistant + tool
// results after that user) keep the model's own reasoning_content. Encrypted
// or signed blobs are never removed. This function only deletes fields; it
// never injects placeholders such as "tool call".
//
// Grok Build still stores and displays Thought from the response path.
func stripChatHistoryReasoning(root map[string]any) {
	if root == nil {
		return
	}
	messages := anySlice(root["messages"])
	lastUser := chatLastUserIndex(messages)
	for index, raw := range messages {
		if index >= lastUser {
			continue
		}
		msg, _ := raw.(map[string]any)
		if msg == nil || !strings.EqualFold(stringValue(msg["role"]), "assistant") {
			continue
		}
		stripPlaintextChatThought(msg)
	}
}

func chatLastUserIndex(messages []any) int {
	last := -1
	for index, raw := range messages {
		msg, _ := raw.(map[string]any)
		if msg != nil && strings.EqualFold(stringValue(msg["role"]), "user") {
			last = index
		}
	}
	return last
}

func stripPlaintextChatThought(msg map[string]any) {
	if msg == nil {
		return
	}
	for _, key := range chatThoughtKeys {
		delete(msg, key)
	}
	if details, ok := keepOpaqueReasoningDetails(msg["reasoning_details"]); ok {
		msg["reasoning_details"] = details
	} else {
		delete(msg, "reasoning_details")
	}
	content, ok := msg["content"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(content))
	for _, block := range content {
		item, _ := block.(map[string]any)
		if item != nil && isPlaintextThoughtBlock(item) {
			continue
		}
		filtered = append(filtered, block)
	}
	msg["content"] = filtered
}

func isPlaintextThoughtBlock(item map[string]any) bool {
	if item == nil || thoughtBlockIsOpaque(item) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
	case "thinking", "reasoning", "thought", "reasoning_text", "redacted_thinking":
		return true
	default:
		return false
	}
}

func thoughtBlockIsOpaque(item map[string]any) bool {
	if item == nil {
		return false
	}
	if firstString(item, "signature", "encrypted_content") != "" {
		return true
	}
	typ := strings.ToLower(firstString(item, "type", "kind"))
	return strings.Contains(typ, "encrypt") || strings.Contains(typ, "redact")
}

func keepOpaqueReasoningDetails(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	parts := anySlice(value)
	if len(parts) == 0 {
		return nil, false
	}
	kept := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if thoughtBlockIsOpaque(part) {
			kept = append(kept, raw)
		}
	}
	if len(kept) == 0 {
		return nil, false
	}
	return kept, true
}

func collectChatThought(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	// Grok Build only reads reasoning_content. Prefer that field and do not
	// concatenate every vendor alias: joining trimmed fragments is what glued
	// "Check"+" git" into "Checkgit".
	for _, key := range chatThoughtKeys {
		if _, ok := obj[key]; !ok {
			continue
		}
		if text := thoughtValueText(obj[key]); text != "" {
			return text
		}
	}
	return reasoningDetailsText(obj["reasoning_details"])
}

func reasoningDetailsText(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	var parts []string
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if part == nil {
			if s, ok := raw.(string); ok && strings.TrimSpace(s) != "" {
				parts = append(parts, s)
			}
			continue
		}
		typ := strings.ToLower(firstString(part, "type", "kind"))
		if strings.Contains(typ, "encrypt") || strings.Contains(typ, "redact") {
			continue
		}
		text := firstString(part, "text", "summary", "content", "reasoning")
		if text == "" {
			text = thoughtValueText(part)
		}
		if strings.TrimSpace(text) != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "")
}

func appendUniqueThought(dst, src string) string {
	if src == "" {
		return dst
	}
	if dst == "" {
		return src
	}
	if dst == src || strings.Contains(dst, src) {
		return dst
	}
	if strings.Contains(src, dst) {
		return src
	}
	return dst + src
}

func wrapToolCallsArray(obj map[string]any) {
	if obj == nil {
		return
	}
	raw, present := obj["tool_calls"]
	if !present || raw == nil {
		return
	}
	if _, ok := raw.([]any); ok {
		return
	}
	if call, ok := raw.(map[string]any); ok {
		obj["tool_calls"] = []any{call}
	}
}

func liftGeminiFunctionCall(obj map[string]any) {
	if obj == nil || len(anySlice(obj["tool_calls"])) > 0 {
		delete(obj, "functionCall")
		return
	}
	raw, ok := obj["functionCall"]
	delete(obj, "functionCall")
	if !ok || raw == nil {
		return
	}
	call, _ := raw.(map[string]any)
	if call == nil {
		return
	}
	wrapped := cloneMap(call)
	if wrapped["args"] != nil && wrapped["arguments"] == nil {
		wrapped["arguments"] = wrapped["args"]
		delete(wrapped, "args")
	}
	liftChatToolCallObject(wrapped)
	if stringValue(wrapped["id"]) == "" {
		wrapped["id"] = compatID("call")
	}
	obj["tool_calls"] = []any{wrapped}
}
