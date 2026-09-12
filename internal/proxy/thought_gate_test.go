package proxy

import (
	"strings"
	"testing"
)

func TestChatRectifierPreservesReasoningTokenSpaces(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"reasoning_content": "Check"})
	ingest(map[string]any{"reasoning_content": " git"})
	ingest(map[string]any{"reasoning_content": " status."})
	reasoning, _ := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "Check git status." {
		t.Fatalf("streamed reasoning lost BPE spaces: %q frames=%s", strings.Join(reasoning, ""), mustJSON(out))
	}
}

func TestChatRectifierDoesNotGlueDuplicateThoughtKeys(t *testing.T) {
	frames, _ := newChatToolRectifier(nil, "").ingest(chatDeltaChunk(map[string]any{
		"reasoning_content": "Check git",
		"thinking":          "Check git",
	}))
	reasoning, _ := grokBuildChatChannels(frames)
	if strings.Join(reasoning, "") != "Check git" {
		t.Fatalf("duplicate keys glued: %q frames=%s", strings.Join(reasoning, ""), mustJSON(frames))
	}
}

func TestSanitizeThoughtDeltaKeepsLeadingSpace(t *testing.T) {
	if got := sanitizeThoughtDelta(" git"); got != " git" {
		t.Fatalf("delta space stripped: %q", got)
	}
	if got := sanitizeThoughtDelta("previous task is complete."); got != "" {
		t.Fatalf("protocol delta survived: %q", got)
	}
	keep := " I'll continue by reading the file."
	if got := sanitizeThoughtDelta(keep); got != keep {
		t.Fatalf("legitimate delta changed: %q", got)
	}
}

func TestSanitizeThoughtKeepsWorkAndDropsLoopProtocol(t *testing.T) {
	keep := "I'll continue by reading the file, then run the tests."
	if got := sanitizeThought(keep); got != keep {
		t.Fatalf("legitimate continue was stripped: %q", got)
	}
	english := `checking status. The user says "continue. If all tasks are complete, reply only: DONE" — task already fully completed.`
	if got := sanitizeThought(english); got != "checking status." {
		t.Fatalf("english protocol survived: %q", got)
	}
	if sanitizeThought("If all tasks are complete, reply only: 任务已全部完成.") != "" {
		t.Fatal("canned reply-only protocol survived")
	}
	if sanitizeThought("previous task (lint) is complete.") != "" {
		t.Fatal("previous-task terminator survived")
	}
}

func TestChatThoughtAliasesLiftToReasoningContent(t *testing.T) {
	for _, delta := range []map[string]any{
		{"thinking": "plan the lookup"},
		{"reasoning": "plan the lookup"},
		{"reasoning_text": "plan the lookup"},
		{"thought": "plan the lookup"},
		{"reasoning": map[string]any{"text": "plan the lookup"}},
	} {
		rectifier := newChatToolRectifier(nil, "")
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		if len(frames) != 1 {
			t.Fatalf("alias %v frames=%d", delta, len(frames))
		}
		got := frames[0]["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
		if stringValue(got["reasoning_content"]) != "plan the lookup" {
			t.Fatalf("alias %v not lifted: %#v", delta, got)
		}
		for _, key := range []string{"thinking", "reasoning", "reasoning_text", "thought"} {
			if _, exists := got[key]; exists {
				t.Fatalf("vendor key %s leaked: %#v", key, got)
			}
		}
	}
}

func TestChatRectifierSplitsVendorThinkingBeforeContent(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	frames, _ := rectifier.ingest(chatDeltaChunk(map[string]any{
		"thinking": "plan",
		"content":  "ok",
	}))
	if len(frames) != 2 {
		t.Fatalf("expected split, got %d %s", len(frames), mustJSON(frames))
	}
	reasoning, text := grokBuildChatChannels(frames)
	if strings.Join(reasoning, "") != "plan" || strings.Join(text, "") != "ok" {
		t.Fatalf("reasoning=%q text=%q", reasoning, text)
	}
}

func TestChatRectifierDropsPostContentVendorThinking(t *testing.T) {
	rectifier := newChatToolRectifier(nil, "")
	var out []map[string]any
	ingest := func(delta map[string]any) {
		frames, _ := rectifier.ingest(chatDeltaChunk(delta))
		out = append(out, frames...)
	}
	ingest(map[string]any{"thinking": "tree is clean"})
	ingest(map[string]any{"content": "没有变更。"})
	ingest(map[string]any{"reasoning": "The user says continue. If all tasks are complete, reply only: DONE."})
	reasoning, text := grokBuildChatChannels(out)
	if strings.Join(reasoning, "") != "tree is clean" {
		t.Fatalf("reasoning=%q frames=%s", reasoning, mustJSON(out))
	}
	if strings.Join(text, "") != "没有变更。" {
		t.Fatalf("text=%q", text)
	}
	body := mustJSON(out)
	if strings.Contains(body, "reply only") || strings.Contains(body, "DONE") {
		t.Fatalf("post-content thinking leaked: %s", body)
	}
}

func TestMessagesThoughtRectifierDropsThinkingAfterText(t *testing.T) {
	r := newMessagesThoughtRectifier()
	if !r.keep(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "thinking", "thinking": "inspect git"},
	}) {
		t.Fatal("prefix thinking dropped")
	}
	if !r.keep(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": " status"},
	}) {
		t.Fatal("prefix thinking delta dropped")
	}
	if !r.keep(map[string]any{"type": "content_block_stop", "index": 0}) {
		t.Fatal("prefix thinking stop dropped")
	}
	if !r.keep(map[string]any{
		"type": "content_block_start", "index": 1,
		"content_block": map[string]any{"type": "text", "text": ""},
	}) {
		t.Fatal("text start dropped")
	}
	if !r.keep(map[string]any{
		"type": "content_block_delta", "index": 1,
		"delta": map[string]any{"type": "text_delta", "text": "干净。"},
	}) {
		t.Fatal("text delta dropped")
	}
	if r.keep(map[string]any{
		"type": "content_block_start", "index": 2,
		"content_block": map[string]any{"type": "thinking", "thinking": "previous task is complete. reply only: DONE"},
	}) {
		t.Fatal("post-text thinking start forwarded")
	}
	if r.keep(map[string]any{
		"type": "content_block_delta", "index": 2,
		"delta": map[string]any{"type": "thinking_delta", "thinking": " more"},
	}) {
		t.Fatal("post-text thinking delta forwarded")
	}
	if r.keep(map[string]any{"type": "content_block_stop", "index": 2}) {
		t.Fatal("post-text thinking stop forwarded")
	}
}

func TestAlignMessagesContentDropsTrailingThinking(t *testing.T) {
	got := alignMessagesContentThoughts([]any{
		map[string]any{"type": "thinking", "thinking": "inspect git. previous task is complete. The user says continue."},
		map[string]any{"type": "text", "text": "干净。"},
		map[string]any{"type": "thinking", "thinking": "If all tasks are complete, reply only: DONE."},
	})
	if len(got) != 2 {
		t.Fatalf("len=%d %s", len(got), mustJSON(got))
	}
	if stringValue(got[0].(map[string]any)["thinking"]) != "inspect git." {
		t.Fatalf("prefix thinking=%#v", got[0])
	}
	if stringValue(got[1].(map[string]any)["type"]) != "text" {
		t.Fatalf("text lost: %s", mustJSON(got))
	}
}

func TestAlignMessagesContentPeelsThinkTagsFromText(t *testing.T) {
	got := alignMessagesContentThoughts([]any{
		map[string]any{"type": "text", "text": "<think>inspect git</think>干净。"},
	})
	if len(got) != 2 {
		t.Fatalf("len=%d %s", len(got), mustJSON(got))
	}
	if stringValue(got[0].(map[string]any)["thinking"]) != "inspect git" {
		t.Fatalf("thinking=%#v", got[0])
	}
	if stringValue(got[1].(map[string]any)["text"]) != "干净。" {
		t.Fatalf("text=%#v", got[1])
	}
}

func TestResponsesThoughtRectifierDropsReasoningAfterText(t *testing.T) {
	r := newResponsesThoughtRectifier()
	if !r.keep(map[string]any{"type": "response.reasoning_text.delta", "delta": "inspect git"}) {
		t.Fatal("prefix reasoning dropped")
	}
	if !r.keep(map[string]any{"type": "response.output_text.delta", "delta": "干净。"}) {
		t.Fatal("output text dropped")
	}
	if r.keep(map[string]any{"type": "response.reasoning_text.delta", "delta": "If all tasks are complete, reply only: DONE."}) {
		t.Fatal("post-text reasoning delta forwarded")
	}
	if r.keep(map[string]any{
		"type": "response.output_item.added",
		"item": map[string]any{"type": "reasoning", "id": "rs_late", "content": []any{map[string]any{"type": "reasoning_text", "text": "task already fully completed"}}},
	}) {
		t.Fatal("post-text reasoning item forwarded")
	}
	completed := map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"output": []any{
				map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "inspect git. previous task is complete."}}},
				map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "干净。"}}},
				map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": "reply only: DONE"}}},
			},
		},
	}
	if !r.keep(completed) {
		t.Fatal("completed dropped")
	}
	output := anySlice(completed["response"].(map[string]any)["output"])
	first := output[0].(map[string]any)
	summary := anySlice(first["summary"])
	if len(summary) != 1 || stringValue(summary[0].(map[string]any)["text"]) != "inspect git." {
		t.Fatalf("prefix summary=%s", mustJSON(first))
	}
	late := output[2].(map[string]any)
	if len(anySlice(late["content"])) != 0 {
		t.Fatalf("trailing reasoning kept visible: %s", mustJSON(late))
	}
}

func TestCanonicalFromChatLiftsThinkingAlias(t *testing.T) {
	body := []byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok","thinking":"plan it. If all tasks are complete, reply only: DONE."},"finish_reason":"stop"}]}`)
	result, err := canonicalFromChat(body, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Output) < 2 {
		t.Fatalf("output=%s", mustJSON(result.Output))
	}
	text := stringValue(anySlice(result.Output[0].(map[string]any)["content"])[0].(map[string]any)["text"])
	if text != "plan it." {
		t.Fatalf("thinking alias=%q", text)
	}
}
