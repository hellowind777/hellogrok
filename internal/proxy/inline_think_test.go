package proxy

import (
	"strings"
	"testing"
)

func TestSplitLeadingThinkBlock(t *testing.T) {
	thought, rest, ok := splitLeadingThinkBlock("  <think>\nplan the lookup\n</think>\n没有变更。")
	if !ok || thought != "plan the lookup" || rest != "没有变更。" {
		t.Fatalf("thought=%q rest=%q ok=%t", thought, rest, ok)
	}
	if _, _, ok := splitLeadingThinkBlock("<think>still open"); ok {
		t.Fatal("unclosed think was split")
	}
	if _, _, ok := splitLeadingThinkBlock("plain answer"); ok {
		t.Fatal("plain text treated as think")
	}
}

func TestLiftChatWirePeelsThinkTagsFromContent(t *testing.T) {
	delta := map[string]any{
		"content": "<think>inspect git</think>\n没有变更。",
	}
	liftChatWireDialect(delta)
	if stringValue(delta["reasoning_content"]) != "inspect git" {
		t.Fatalf("reasoning=%q", delta["reasoning_content"])
	}
	if stringValue(delta["content"]) != "没有变更。" {
		t.Fatalf("content=%q", delta["content"])
	}
}

func TestChatRectifierSplitsStreamedThinkTags(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"content": "<th"})
	ingest(map[string]any{"content": "ink>tree is clean"})
	ingest(map[string]any{"content": "</think>没有变更。"})
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "tree is clean" {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "没有变更。" {
		t.Fatalf("text=%q frames=%s", text, mustJSON(out))
	}
	for i, frame := range out {
		if err := validateNativeSSEFrame(wireChatCompletions, frame); err != nil {
			t.Fatalf("frame %d invalid: %v body=%s", i, err, mustJSON(frame))
		}
	}
}

func TestChatRectifierFlushesUnclosedThinkOnFinish(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{"content": "<think>still planning"}))
	out = append(out, frames...)
	frames, _ = rectifier.ingest(chatFinishChunk("stop"))
	out = append(out, frames...)
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "still planning" {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "" {
		t.Fatalf("text leaked=%q", text)
	}
}

func TestChatRectifierDoesNotTreatOrdinaryContentAsThink(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{"content": "hello"}))
	reasoning, text := grokBuildChatChannels(frames)
	if strings.Join(reasoning, "") != "" || strings.Join(text, "") != "hello" {
		t.Fatalf("reasoning=%q text=%q", reasoning, text)
	}
}

func TestCanonicalFromChatPeelsThinkTags(t *testing.T) {
	body := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"<think>plan it.</think>ok"},"finish_reason":"stop"}]}`)
	result, err := canonicalFromChat(body, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) < 2 {
		t.Fatalf("output=%s", mustJSON(result.Output))
	}
	thought := stringValue(anySlice(result.Output[0].(map[string]any)["content"])[0].(map[string]any)["text"])
	if thought != "plan it." {
		t.Fatalf("thought=%q", thought)
	}
	text := stringValue(anySlice(result.Output[1].(map[string]any)["content"])[0].(map[string]any)["text"])
	if text != "ok" {
		t.Fatalf("text=%q", text)
	}
}

// Thinking models emit several CoT phases per turn; a second think span that
// starts after visible reply text must never reach the client as answer text.
// The thought gate drops reasoning that arrives after the reply, so the span
// is expected to disappear entirely instead of opening a second Thought.
func TestChatRectifierStripsSecondPhaseThinkSpan(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"content": "answer one."})
	ingest(map[string]any{"content": " <think>re-check the diff</think>done."})
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "" {
		t.Fatalf("late reasoning must be dropped, got %q", reasoning)
	}
	if joined := strings.Join(text, ""); joined != "answer one.done." {
		t.Fatalf("text=%q frames=%s", joined, mustJSON(out))
	}
}

// A relay that misroutes the reasoning tail into content leaves only the
// closing tag there; the text before it is CoT, not reply text.
func TestChatRectifierClassifiesUnbalancedThinkCloseAsReasoning(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{"content": "reasoning tail.\n</think>real reply"}))
	out = append(out, frames...)
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "reasoning tail." {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "real reply" {
		t.Fatalf("text=%q", text)
	}
}

// A stray closing tag in already-latched reply text must never reach the
// client verbatim, and a tag split across deltas must not leak either.
func TestChatRectifierNeverShowsStrayOrSplitThinkTags(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"content": "hello "})
	ingest(map[string]any{"content": "world </think> bye"})
	ingest(map[string]any{"content": "a </thi"})
	ingest(map[string]any{"content": "nk> b"})
	reasoning, text := grokBuildChatChannels(out)
	joined := strings.Join(text, "")
	if strings.Contains(joined, "</think>") || strings.Contains(joined, "</thi") {
		t.Fatalf("think tag leaked into text: %q", joined)
	}
	if joined != "hello world  byea  b" {
		t.Fatalf("text=%q", joined)
	}
	if strings.Join(reasoning, "") != "" {
		t.Fatalf("reasoning=%q", reasoning)
	}
}

func TestPeelThinkFromChatContentStripsEverySpan(t *testing.T) {
	obj := map[string]any{"content": "<think>a</think>mid<think>b</think>end"}
	peelThinkFromChatContent(obj)
	if stringValue(obj["content"]) != "midend" {
		t.Fatalf("content=%q", obj["content"])
	}
	if stringValue(obj["reasoning_content"]) != "a\nb" {
		t.Fatalf("reasoning=%q", obj["reasoning_content"])
	}
}

// Reasoning that discusses the tags quotes them; a quoted closing tag must
// not terminate the span early, or the remainder of the thought leaks into
// the visible reply.
func TestChatRectifierKeepsQuotedTagsInsideReasoningSpan(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{"content": "discuss the `` literal and more</think>reply"}))
	out = append(out, frames...)
	reasoning, text := grokBuildChatChannels(out)
	if joined := strings.Join(reasoning, ""); !strings.Contains(joined, "``") || !strings.Contains(joined, "and more") {
		t.Fatalf("reasoning truncated at quoted tag: %q", joined)
	}
	if strings.Join(text, "") != "reply" {
		t.Fatalf("text=%q", text)
	}
}

// Quoted tags in visible reply text are prose about the tags and must reach
// the client intact; only unquoted stray closing tags are removed.
func TestChatRectifierKeepsQuotedTagsInVisibleText(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{"content": "see `` and \"</think>\" here"}))
	out = append(out, frames...)
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "" {
		t.Fatalf("reasoning=%q", reasoning)
	}
	if joined := strings.Join(text, ""); joined != "see `` and \"</think>\" here" {
		t.Fatalf("quoted tags mangled: %q", joined)
	}
}

func TestPeelThinkFromChatContentKeepsQuotedTags(t *testing.T) {
	obj := map[string]any{"content": "<think>a</think>mid `` end"}
	peelThinkFromChatContent(obj)
	if stringValue(obj["content"]) != "mid `` end" {
		t.Fatalf("content=%q", obj["content"])
	}
	if stringValue(obj["reasoning_content"]) != "a" {
		t.Fatalf("reasoning=%q", obj["reasoning_content"])
	}
}
