package proxy

import "strings"

const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
)

type inlineThinkMode int

const (
	inlineThinkDetecting inlineThinkMode = iota
	inlineThinkReasoning
	inlineThinkText
)

// inlineThinkState is CC Switch's Chat→Responses think-tag state machine.
// Many Chat-compatible models (Qwen, DeepSeek-R1, local forks) wrap CoT in
// <think>…</think> inside content instead of a reasoning field. Grok Build
// must see that as a prefix Reasoning sibling, not as answer text. Thinking
// models emit several CoT phases per turn and relays sometimes misroute a
// reasoning tail into content with only the closing tag, so spans are
// stripped wherever they appear in the stream, not just at its head.
type inlineThinkState struct {
	mode inlineThinkMode
	buf  strings.Builder
}

func splitLeadingThinkBlock(text string) (thought, rest string, ok bool) {
	leading := len(text) - len(strings.TrimLeft(text, " \t\r\n"))
	after := text[leading:]
	if !strings.HasPrefix(after, thinkOpenTag) {
		return "", text, false
	}
	bodyStart := leading + len(thinkOpenTag)
	rel := strings.Index(text[bodyStart:], thinkCloseTag)
	if rel < 0 {
		return "", text, false
	}
	closeStart := bodyStart + rel
	thought = strings.TrimSpace(text[bodyStart:closeStart])
	rest = strings.TrimLeft(text[closeStart+len(thinkCloseTag):], " \t\r\n")
	return thought, rest, true
}

// splitThinkSpan cuts the first balanced <think>…</think> span anywhere in
// text, reporting the visible text that precedes it and the text after it.
func splitThinkSpan(text string) (pre, thought, rest string, ok bool) {
	openIdx := strings.Index(text, thinkOpenTag)
	if openIdx < 0 {
		return "", "", text, false
	}
	bodyStart := openIdx + len(thinkOpenTag)
	rel := strings.Index(text[bodyStart:], thinkCloseTag)
	if rel < 0 {
		return "", "", text, false
	}
	closeStart := bodyStart + rel
	return text[:openIdx], strings.TrimSpace(text[bodyStart:closeStart]), text[closeStart+len(thinkCloseTag):], true
}

// heldTagSuffixLen reports how many trailing bytes of s form an incomplete
// think tag, so a tag split across stream deltas is never emitted as text.
func heldTagSuffixLen(s string) int {
	best := 0
	for _, tag := range []string{thinkOpenTag, thinkCloseTag} {
		for n := len(tag) - 1; n > 0; n-- {
			if strings.HasSuffix(s, tag[:n]) && n > best {
				best = n
			}
		}
	}
	return best
}

func leadingThinkDecision(buffer string) inlineThinkMode {
	trimmed := strings.TrimLeft(buffer, " \t\r\n")
	if trimmed == "" {
		return inlineThinkDetecting
	}
	if strings.HasPrefix(trimmed, thinkOpenTag) {
		return inlineThinkReasoning
	}
	if strings.HasPrefix(thinkOpenTag, trimmed) {
		return inlineThinkDetecting
	}
	return inlineThinkText
}

func (s *inlineThinkState) feed(delta string) (reasoning, text string, hold bool) {
	if s == nil || delta == "" {
		return "", delta, false
	}
	s.buf.WriteString(delta)
	for {
		buffered := s.buf.String()
		switch s.mode {
		case inlineThinkDetecting:
			switch leadingThinkDecision(buffered) {
			case inlineThinkDetecting:
				return reasoning, text, reasoning == "" && text == ""
			case inlineThinkReasoning:
				s.mode = inlineThinkReasoning
				continue
			}
			closeIdx := strings.Index(buffered, thinkCloseTag)
			openIdx := strings.Index(buffered, thinkOpenTag)
			switch {
			case closeIdx >= 0 && (openIdx < 0 || closeIdx < openIdx):
				// A relay that misrouted the reasoning tail into content
				// leaves only the closing tag; the text before it is CoT.
				reasoning += strings.TrimSpace(buffered[:closeIdx])
				s.mode = inlineThinkText
				s.buf.Reset()
				s.buf.WriteString(buffered[closeIdx+len(thinkCloseTag):])
			case openIdx >= 0:
				pre, thought, rest, _ := splitThinkSpan(buffered)
				text += pre
				reasoning += thought
				s.mode = inlineThinkText
				s.buf.Reset()
				s.buf.WriteString(rest)
			default:
				s.mode = inlineThinkText
			}
		case inlineThinkReasoning:
			thought, rest, ok := splitLeadingThinkBlock(buffered)
			if !ok {
				return reasoning, text, reasoning == "" && text == ""
			}
			reasoning += thought
			s.mode = inlineThinkText
			s.buf.Reset()
			s.buf.WriteString(rest)
		default:
			if pre, thought, rest, ok := splitThinkSpan(buffered); ok {
				text += pre
				reasoning += thought
				s.buf.Reset()
				s.buf.WriteString(rest)
				continue
			}
			if strings.Contains(buffered, thinkCloseTag) {
				buffered = strings.ReplaceAll(buffered, thinkCloseTag, "")
			}
			keep := heldTagSuffixLen(buffered)
			text += buffered[:len(buffered)-keep]
			s.buf.Reset()
			s.buf.WriteString(buffered[len(buffered)-keep:])
			return reasoning, text, false
		}
	}
}

func (s *inlineThinkState) flush() (reasoning, text string) {
	if s == nil {
		return "", ""
	}
	buffered := s.buf.String()
	s.buf.Reset()
	switch s.mode {
	case inlineThinkDetecting:
		s.mode = inlineThinkText
		return "", buffered
	case inlineThinkReasoning:
		s.mode = inlineThinkText
		if thought, rest, ok := splitLeadingThinkBlock(buffered); ok {
			return thought, rest
		}
		trimmed := strings.TrimLeft(buffered, " \t\r\n")
		if strings.HasPrefix(trimmed, thinkOpenTag) {
			return strings.TrimSpace(trimmed[len(thinkOpenTag):]), ""
		}
		return "", buffered
	default:
		return "", buffered
	}
}

func peelThinkFromChatContent(obj map[string]any) {
	if obj == nil {
		return
	}
	raw, ok := obj["content"].(string)
	if !ok || raw == "" {
		return
	}
	var thoughts []string
	var visible strings.Builder
	leadingTrim := false
	rest := raw
	for {
		pre, thought, next, ok := splitThinkSpan(rest)
		if !ok {
			// A stray closing tag is stripped only once something visible
			// precedes it in this object; at the head of a stream delta the
			// turn-level state machine owns the classification.
			if strings.Contains(rest, thinkCloseTag) && (visible.Len() > 0 || len(thoughts) > 0) {
				rest = strings.ReplaceAll(rest, thinkCloseTag, "")
			}
			visible.WriteString(rest)
			break
		}
		if visible.Len() == 0 && len(thoughts) == 0 && strings.TrimSpace(pre) == "" {
			leadingTrim = true
		}
		visible.WriteString(pre)
		thoughts = append(thoughts, thought)
		rest = next
	}
	shown := visible.String()
	if leadingTrim {
		shown = strings.TrimLeft(shown, " \t\r\n")
	}
	if shown == "" {
		delete(obj, "content")
	} else {
		obj["content"] = shown
	}
	if thought := strings.TrimSpace(strings.Join(thoughts, "\n")); thought != "" {
		writeChatReasoning(obj, appendUniqueThought(stringValue(obj["reasoning_content"]), thought))
	}
}

func mergeChatThinkDelta(delta map[string]any, reasoning, text string) {
	if delta == nil {
		return
	}
	if reasoning != "" {
		writeChatReasoning(delta, appendUniqueThought(stringValue(delta["reasoning_content"]), reasoning))
	}
	if text == "" {
		if _, present := delta["content"]; present {
			delete(delta, "content")
		}
		return
	}
	delta["content"] = text
}
