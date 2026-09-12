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
