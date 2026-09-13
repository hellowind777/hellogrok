package proxy

import (
	"sort"
	"strings"
)

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
	// unknownReasoning collects the distinct event types intercepted by the
	// fail-loud default so the caller logs each once per stream, not once
	// per frame of a chatty unregistered delta.
	unknownReasoning map[string]struct{}
}

func newResponsesThoughtRectifier() *responsesThoughtRectifier {
	return &responsesThoughtRectifier{
		droppedItems:     map[string]struct{}{},
		unknownReasoning: map[string]struct{}{},
	}
}

// The Responses thought gate is table-driven: every event type the gate
// understands has exactly one rule, and an event carrying visible reasoning
// that has no rule is intercepted (fail-loud) rather than passed through
// (fail-open). A missing rule costs one log line and a missing thought; a
// missing gate costs a second Thought rendered under the user's reply, which
// is the regression this component exists to prevent. Register every new
// Responses reasoning event type here when it appears in the wild.
type responsesThoughtRule struct {
	// visibleReasoning reports whether an event of this type can carry
	// reasoning text the user could see. Only such events participate in
	// droppedItems filtering and the fail-loud default.
	visibleReasoning bool
	// apply mutates the event in place and reports whether it may pass.
	apply func(r *responsesThoughtRectifier, event map[string]any) bool
}

// isReasoningItemType reports whether a Responses output item or content
// part type carries visible reasoning. The official type is "reasoning";
// relays that bridge Anthropic-style blocks through protocol conversion emit
// "thinking" and summary/redacted variants in the same slot, and those must
// pass through the same gate instead of leaking ungated.
func isReasoningItemType(typ string) bool {
	switch typ {
	case "reasoning", "thinking", "reasoning_summary", "redacted_thinking":
		return true
	default:
		return false
	}
}

func reasoningItemEvent(itemKey string, markDropped bool) responsesThoughtRule {
	return responsesThoughtRule{
		visibleReasoning: true,
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			item, _ := event[itemKey].(map[string]any)
			if !isReasoningItemType(stringValue(item["type"])) {
				return true
			}
			if r.gate.sawText {
				if markDropped {
					if id := stringValue(item["id"]); id != "" {
						r.droppedItems[id] = struct{}{}
					}
				}
				if stringValue(item["encrypted_content"]) == "" {
					return false
				}
				item["summary"] = []any{}
				if _, exists := item["content"]; exists {
					item["content"] = []any{}
				}
				return true
			}
			sanitizeResponsesReasoningItem(item)
			return true
		},
	}
}

func reasoningTextEvent(field string, isDelta bool) responsesThoughtRule {
	return responsesThoughtRule{
		visibleReasoning: true,
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			cleaned, ok := r.gate.accept(stringValue(event[field]))
			if !ok {
				if isDelta {
					return false
				}
				event[field] = ""
				return !r.gate.sawText
			}
			event[field] = cleaned
			return true
		},
	}
}

func outputTextEvent(field string) responsesThoughtRule {
	return responsesThoughtRule{
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			r.gate.noteText(stringValue(event[field]))
			return true
		},
	}
}

func reasoningSummaryPartEvent() responsesThoughtRule {
	return responsesThoughtRule{
		visibleReasoning: true,
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			part, _ := event["part"].(map[string]any)
			if r.gate.sawText {
				return false
			}
			if text := stringValue(part["text"]); text != "" {
				if cleaned, ok := r.gate.accept(text); ok {
					part["text"] = cleaned
				} else {
					part["text"] = ""
				}
			}
			return true
		},
	}
}

func contentPartEvent() responsesThoughtRule {
	return responsesThoughtRule{
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			part, _ := event["part"].(map[string]any)
			partType := stringValue(part["type"])
			if partType == "text" && r.gate.sawText {
				// A post-answer content part that is entirely a <think> block
				// is late reasoning in disguise; drop it. A part mixing
				// visible text with a think block keeps the text.
				if _, rest, ok := splitLeadingThinkBlock(stringValue(part["text"])); ok && rest == "" {
					return false
				}
				return true
			}
			if partType != "reasoning_text" && !isReasoningItemType(partType) {
				return true
			}
			if r.gate.sawText {
				return false
			}
			if text := stringValue(part["text"]); text != "" {
				if cleaned, ok := r.gate.accept(text); ok {
					part["text"] = cleaned
				} else {
					part["text"] = ""
				}
			}
			return true
		},
	}
}

func terminalResponseEvent() responsesThoughtRule {
	return responsesThoughtRule{
		apply: func(r *responsesThoughtRectifier, event map[string]any) bool {
			if response, _ := event["response"].(map[string]any); response != nil {
				alignResponsesOutputThoughts(response)
			}
			return true
		},
	}
}

var responsesThoughtRules = map[string]responsesThoughtRule{
	"response.output_text.delta":            outputTextEvent("delta"),
	"response.output_text.done":             outputTextEvent("text"),
	"response.output_item.added":            reasoningItemEvent("item", true),
	"response.output_item.done":             reasoningItemEvent("item", false),
	"response.reasoning_text.delta":         reasoningTextEvent("delta", true),
	"response.reasoning_summary_text.delta": reasoningTextEvent("delta", true),
	"response.reasoning_text.done":          reasoningTextEvent("text", false),
	"response.reasoning_summary_text.done":  reasoningTextEvent("text", false),
	"response.reasoning_summary_part.added": reasoningSummaryPartEvent(),
	"response.reasoning_summary_part.done":  reasoningSummaryPartEvent(),
	"response.content_part.added":           contentPartEvent(),
	"response.content_part.done":            contentPartEvent(),
	"response.completed":                    terminalResponseEvent(),
	"response.incomplete":                   terminalResponseEvent(),
	"response.failed":                       terminalResponseEvent(),
}

func (r *responsesThoughtRectifier) keep(event map[string]any) bool {
	if r == nil || event == nil {
		return true
	}
	typ := stringValue(event["type"])
	rule, known := responsesThoughtRules[typ]
	if !known {
		if looksLikeReasoningEvent(event) {
			// Fail-loud: an unregistered reasoning carrier is intercepted and
			// reported instead of leaking a second Thought under the reply.
			r.unknownReasoning[typ] = struct{}{}
			return false
		}
		return true
	}
	if rule.visibleReasoning {
		if itemID := firstString(event, "item_id"); itemID != "" {
			if _, dropped := r.droppedItems[itemID]; dropped {
				return false
			}
		}
	}
	return rule.apply(r, event)
}

// looksLikeReasoningEvent reports whether an unregistered event type plausibly
// carries visible reasoning: a reasoning-* type, or an item/part whose own
// type marker names reasoning or thinking.
func looksLikeReasoningEvent(event map[string]any) bool {
	typ := stringValue(event["type"])
	if strings.Contains(typ, "reasoning") || strings.Contains(typ, "thinking") {
		return true
	}
	for _, key := range []string{"item", "part", "delta", "content_block"} {
		nested, _ := event[key].(map[string]any)
		nestedType := stringValue(nested["type"])
		if isReasoningItemType(nestedType) || strings.Contains(nestedType, "reasoning") || strings.Contains(nestedType, "thinking") {
			return true
		}
	}
	return false
}

// takeUnknownReasoning drains the intercepted event types for one log line
// at end of stream.
func (r *responsesThoughtRectifier) takeUnknownReasoning() []string {
	if r == nil || len(r.unknownReasoning) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.unknownReasoning))
	for typ := range r.unknownReasoning {
		out = append(out, typ)
	}
	sort.Strings(out)
	clear(r.unknownReasoning)
	return out
}

// noteUnknownReasoningEvents logs the fail-loud interceptions of one stream.
func noteUnknownReasoningEvents(logf func(string, ...any), channel string, rectifier *responsesThoughtRectifier) {
	if logf == nil {
		return
	}
	for _, typ := range rectifier.takeUnknownReasoning() {
		logf("UP channel=%s intercepted unregistered reasoning event type %q; register it in responsesThoughtRules", channel, typ)
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
		itemType := stringValue(item["type"])
		switch {
		case isReasoningItemType(itemType):
			// Relay thinking variants ride the same output slot as official
			// reasoning; the terminal frame must apply the same sanitize and
			// post-answer clearing as the streamed frames did, or a dropped
			// late thought reappears in full here.
			sanitizeResponsesReasoningItem(item)
			if gate.sawText {
				item["summary"] = []any{}
				if _, exists := item["content"]; exists {
					item["content"] = []any{}
				}
			}
		case itemType == "message":
			for _, rawPart := range anySlice(item["content"]) {
				part, _ := rawPart.(map[string]any)
				if typ := stringValue(part["type"]); typ == "output_text" || typ == "text" {
					gate.noteText(stringValue(part["text"]))
				}
			}
		}
	}
}
