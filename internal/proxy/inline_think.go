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
// must see that as a prefix Reasoning sibling, not as answer text.
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
	switch s.mode {
	case inlineThinkText:
		return "", delta, false
	case inlineThinkDetecting:
		s.buf.WriteString(delta)
		switch leadingThinkDecision(s.buf.String()) {
		case inlineThinkDetecting:
			return "", "", true
		case inlineThinkReasoning:
			s.mode = inlineThinkReasoning
			return s.drain()
		default:
			s.mode = inlineThinkText
			text = s.buf.String()
			s.buf.Reset()
			return "", text, false
		}
	default:
		s.buf.WriteString(delta)
		return s.drain()
	}
}

func (s *inlineThinkState) drain() (reasoning, text string, hold bool) {
	buffered := s.buf.String()
	thought, rest, ok := splitLeadingThinkBlock(buffered)
	if !ok {
		return "", "", true
	}
	s.mode = inlineThinkText
	s.buf.Reset()
	return thought, rest, false
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
		return "", ""
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
	thought, rest, ok := splitLeadingThinkBlock(raw)
	if !ok {
		return
	}
	if rest == "" {
		delete(obj, "content")
	} else {
		obj["content"] = rest
	}
	if thought != "" {
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
