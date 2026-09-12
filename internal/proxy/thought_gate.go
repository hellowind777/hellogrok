package proxy

import "strings"

// thoughtGate enforces Grok Build's conversation order [Reasoning, Assistant]
// for every third-party channel and wire format. The TUI finishes the Thought
// block on the first AgentMessageChunk; a later thought opens a second Thought
// under the reply. Official grok streams reasoning as a prefix sibling.
type thoughtGate struct {
	sawText bool
}

func (g *thoughtGate) noteText(text string) {
	if g != nil && strings.TrimSpace(text) != "" {
		g.sawText = true
	}
}

func (g *thoughtGate) accept(text string) (string, bool) {
	if text == "" || (g != nil && g.sawText) {
		return "", false
	}
	if strings.TrimSpace(text) == "" {
		return text, true
	}
	cleaned := sanitizeThoughtDelta(text)
	if cleaned == "" {
		return "", false
	}
	return cleaned, true
}

// Vendor Chat Completions thinking fields. Grok Build's sampler only reads
// delta.reasoning_content; every other key is ignored on the wire.
var chatThoughtKeys = []string{
	"reasoning_content",
	"reasoning",
	"thinking",
	"reasoning_text",
	"thought",
	"thoughts",
}

func chatThoughtText(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	for _, key := range chatThoughtKeys {
		if text := thoughtValueText(obj[key]); text != "" {
			return text
		}
	}
	return ""
}

func thoughtValueText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		if _, hasEffort := v["effort"]; hasEffort &&
			v["text"] == nil && v["content"] == nil && v["thinking"] == nil && v["reasoning"] == nil {
			return ""
		}
		return firstString(v, "text", "content", "reasoning_content", "reasoning", "thinking", "summary")
	default:
		var parts []string
		for _, raw := range anySlice(value) {
			if piece := thoughtValueText(raw); piece != "" {
				parts = append(parts, piece)
			}
		}
		return strings.Join(parts, "")
	}
}

func liftChatThoughtFields(obj map[string]any) {
	if obj == nil {
		return
	}
	text := collectChatThought(obj)
	for _, key := range []string{"thinking", "reasoning_text", "thought", "thoughts", "reasoning_details"} {
		delete(obj, key)
	}
	switch typed := obj["reasoning"].(type) {
	case string:
		delete(obj, "reasoning")
	case map[string]any:
		if thoughtValueText(typed) != "" {
			delete(obj, "reasoning")
		}
	case []any:
		delete(obj, "reasoning")
	}
	writeChatReasoning(obj, text)
}

func clearChatReasoning(obj map[string]any) {
	if obj == nil {
		return
	}
	delete(obj, "reasoning_content")
	delete(obj, "reasoning")
	delete(obj, "reasoning_details")
	for _, key := range []string{"thinking", "reasoning_text", "thought", "thoughts"} {
		delete(obj, key)
	}
}

func writeChatReasoning(obj map[string]any, text string) {
	if obj == nil {
		return
	}
	if text == "" {
		clearChatReasoning(obj)
		return
	}
	obj["reasoning_content"] = text
}

func sanitizeChatMessageReasoning(obj map[string]any) {
	if obj == nil {
		return
	}
	liftChatWireDialect(obj)
	writeChatReasoning(obj, sanitizeThought(stringValue(obj["reasoning_content"])))
}

func sanitizeChatReasoning(text string) string {
	return sanitizeThought(text)
}

// sanitizeThoughtDelta is for streaming tokens. Grok Build's Chat L2
// concatenates reasoning_content as-is; trimming each delta eats the leading
// space that BPE tokens use between English words.
func sanitizeThoughtDelta(text string) string {
	if text == "" {
		return ""
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	cleaned := sanitizeThought(trimmed)
	if cleaned == "" {
		return ""
	}
	lead := text[:len(text)-len(strings.TrimLeft(text, " \t\r\n"))]
	trail := text[len(strings.TrimRight(text, " \t\r\n")):]
	return lead + cleaned + trail
}

// sanitizeThought drops agent-loop closing self-talk that thinking models of
// any vendor dump into CoT: a synthetic continue instruction, a "reply only"
// canned token, or a previous-task-complete terminator. Official grok
// summaries never contain this. Legitimate "I'll continue by reading X" is kept.
func sanitizeThought(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	var kept strings.Builder
	for _, sentence := range splitReasoningSentences(text) {
		if isProtocolMetaThought(sentence) {
			continue
		}
		kept.WriteString(sentence)
	}
	out := strings.TrimSpace(kept.String())
	out = strings.Trim(out, `"'`+"“”‘’")
	out = strings.TrimSpace(out)
	if strings.Trim(out, `"'`+"“”‘’—–- \t") == "" {
		return ""
	}
	return out
}

func splitReasoningSentences(text string) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	var sentences []string
	start := 0
	for i := 0; i < len(runes); i++ {
		if !isReasoningSentenceEnd(runes, i) {
			continue
		}
		sentences = append(sentences, string(runes[start:i+1]))
		start = i + 1
	}
	if start < len(runes) {
		sentences = append(sentences, string(runes[start:]))
	}
	return sentences
}

func isReasoningSentenceEnd(runes []rune, i int) bool {
	switch runes[i] {
	case '.', '!', '?', '。', '！', '？':
		if runes[i] == '.' && i+1 < len(runes) && runes[i+1] >= '0' && runes[i+1] <= '9' {
			return false
		}
		return true
	default:
		return false
	}
}

func isProtocolMetaThought(sentence string) bool {
	compact := strings.ToLower(strings.Join(strings.Fields(sentence), " "))
	if compact == "" {
		return false
	}
	compact = strings.NewReplacer("“", `"`, "”", `"`, "‘", `'`, "’", `'`, "：", ":").Replace(compact)
	switch {
	case containsAny(compact, "reply only:", "only reply:", "just reply:", "只回复:", "仅回复:", "只回答:"):
		return true
	case containsAny(compact, "if all tasks are complete", "if all tasks complete", "all tasks are complete, reply", "如果所有任务", "若所有任务已完成"):
		return true
	case (containsAny(compact, "the user says", "user says", "用户说", "用户让", "用户要求") &&
		containsAny(compact, "continue", "继续")):
		return true
	case (containsAny(compact, "previous task", "上一任务", "上一个任务") &&
		containsAny(compact, "complete", "完成")):
		return true
	case containsAny(compact, "task already fully completed", "task already completed", "already fully completed"):
		return true
	case isCannedCompletionToken(compact):
		return true
	default:
		return false
	}
}

func isCannedCompletionToken(compact string) bool {
	token := strings.Trim(compact, `."'!！。`)
	switch token {
	case "任务已全部完成", "all tasks completed", "all tasks complete":
		return true
	default:
		return false
	}
}

func containsAny(s string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(s, part) {
			return true
		}
	}
	return false
}

type messagesThoughtRectifier struct {
	gate    thoughtGate
	dropped map[int]struct{}
}

func newMessagesThoughtRectifier() *messagesThoughtRectifier {
	return &messagesThoughtRectifier{dropped: map[int]struct{}{}}
}

func (r *messagesThoughtRectifier) keep(root map[string]any) bool {
	if r == nil || root == nil {
		return true
	}
	switch stringValue(root["type"]) {
	case "content_block_start":
		index := numberInt(root["index"])
		block, _ := root["content_block"].(map[string]any)
		if block == nil {
			return true
		}
		switch stringValue(block["type"]) {
		case "thinking":
			if r.gate.sawText {
				r.dropped[index] = struct{}{}
				return false
			}
			if cleaned, ok := r.gate.accept(stringValue(block["thinking"])); ok {
				block["thinking"] = cleaned
			} else {
				block["thinking"] = ""
			}
		case "text":
			r.gate.noteText(stringValue(block["text"]))
		}
	case "content_block_delta":
		index := numberInt(root["index"])
		if _, dropped := r.dropped[index]; dropped {
			return false
		}
		delta, _ := root["delta"].(map[string]any)
		if delta == nil {
			return true
		}
		switch stringValue(delta["type"]) {
		case "thinking_delta":
			cleaned, ok := r.gate.accept(stringValue(delta["thinking"]))
			if !ok {
				return false
			}
			delta["thinking"] = cleaned
		case "text_delta":
			r.gate.noteText(stringValue(delta["text"]))
		}
	case "content_block_stop":
		index := numberInt(root["index"])
		if _, dropped := r.dropped[index]; dropped {
			return false
		}
	}
	return true
}

func alignMessagesContentThoughts(content []any) []any {
	if len(content) == 0 {
		return content
	}
	var gate thoughtGate
	out := make([]any, 0, len(content))
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block == nil {
			out = append(out, raw)
			continue
		}
		switch stringValue(block["type"]) {
		case "thinking":
			cleaned, ok := gate.accept(stringValue(block["thinking"]))
			if !ok {
				continue
			}
			block["thinking"] = cleaned
		case "text":
			if thought, rest, ok := splitLeadingThinkBlock(stringValue(block["text"])); ok {
				if cleaned, keep := gate.accept(thought); keep {
					out = append(out, map[string]any{"type": "thinking", "thinking": cleaned})
				}
				if strings.TrimSpace(rest) == "" {
					continue
				}
				block["text"] = rest
			}
			gate.noteText(stringValue(block["text"]))
		}
		out = append(out, block)
	}
	return out
}

type responsesThoughtRectifier struct {
	gate         thoughtGate
	droppedItems map[string]struct{}
}

func newResponsesThoughtRectifier() *responsesThoughtRectifier {
	return &responsesThoughtRectifier{droppedItems: map[string]struct{}{}}
}

func (r *responsesThoughtRectifier) keep(event map[string]any) bool {
	if r == nil || event == nil {
		return true
	}
	typ := stringValue(event["type"])
	itemID := firstString(event, "item_id")
	if itemID != "" {
		if _, dropped := r.droppedItems[itemID]; dropped && isResponsesVisibleReasoningEvent(typ) {
			return false
		}
	}
	switch typ {
	case "response.output_text.delta":
		r.gate.noteText(stringValue(event["delta"]))
	case "response.output_text.done":
		r.gate.noteText(stringValue(event["text"]))
	case "response.output_item.added":
		item, _ := event["item"].(map[string]any)
		if stringValue(item["type"]) != "reasoning" {
			break
		}
		sanitizeResponsesReasoningItem(item)
		if r.gate.sawText {
			if id := stringValue(item["id"]); id != "" {
				r.droppedItems[id] = struct{}{}
			}
			if stringValue(item["encrypted_content"]) == "" {
				return false
			}
			item["summary"] = []any{}
			if _, exists := item["content"]; exists {
				item["content"] = []any{}
			}
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		cleaned, ok := r.gate.accept(stringValue(event["delta"]))
		if !ok {
			return false
		}
		event["delta"] = cleaned
	case "response.reasoning_text.done", "response.reasoning_summary_text.done":
		cleaned, ok := r.gate.accept(stringValue(event["text"]))
		if !ok {
			event["text"] = ""
			if r.gate.sawText {
				return false
			}
			break
		}
		event["text"] = cleaned
	case "response.content_part.added", "response.content_part.done":
		part, _ := event["part"].(map[string]any)
		if stringValue(part["type"]) == "reasoning_text" && r.gate.sawText {
			return false
		}
		if text := stringValue(part["text"]); text != "" && stringValue(part["type"]) == "reasoning_text" {
			if cleaned, ok := r.gate.accept(text); ok {
				part["text"] = cleaned
			} else {
				part["text"] = ""
			}
		}
	case "response.completed", "response.incomplete", "response.failed":
		if response, _ := event["response"].(map[string]any); response != nil {
			alignResponsesOutputThoughts(response)
		}
	}
	return true
}

func isResponsesVisibleReasoningEvent(typ string) bool {
	switch typ {
	case "response.reasoning_text.delta", "response.reasoning_text.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return true
	default:
		return false
	}
}

func responsesReasoningHasVisibleText(item map[string]any) bool {
	if item == nil {
		return false
	}
	for _, raw := range anySlice(item["summary"]) {
		part, _ := raw.(map[string]any)
		if strings.TrimSpace(stringValue(part["text"])) != "" {
			return true
		}
	}
	for _, raw := range anySlice(item["content"]) {
		part, _ := raw.(map[string]any)
		if strings.TrimSpace(stringValue(part["text"])) != "" {
			return true
		}
	}
	return false
}

func sanitizeResponsesReasoningItem(item map[string]any) {
	if item == nil {
		return
	}
	sanitizeReasoningParts(item, "summary")
	sanitizeReasoningParts(item, "content")
}

func sanitizeReasoningParts(item map[string]any, key string) {
	parts := anySlice(item[key])
	if len(parts) == 0 {
		return
	}
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, _ := raw.(map[string]any)
		if part == nil {
			out = append(out, raw)
			continue
		}
		if text := stringValue(part["text"]); text != "" {
			cleaned := sanitizeThought(text)
			if cleaned == "" {
				continue
			}
			part["text"] = cleaned
		}
		out = append(out, part)
	}
	item[key] = out
}

func alignResponsesOutputThoughts(root map[string]any) {
	if root == nil {
		return
	}
	var gate thoughtGate
	for _, raw := range anySlice(root["output"]) {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "reasoning":
			sanitizeResponsesReasoningItem(item)
			if gate.sawText {
				item["summary"] = []any{}
				if _, exists := item["content"]; exists {
					item["content"] = []any{}
				}
			}
		case "message":
			for _, rawPart := range anySlice(item["content"]) {
				part, _ := rawPart.(map[string]any)
				if typ := stringValue(part["type"]); typ == "output_text" || typ == "text" {
					gate.noteText(stringValue(part["text"]))
				}
			}
		}
	}
}
