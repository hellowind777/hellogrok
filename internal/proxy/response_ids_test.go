package proxy

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/patch"
)

func TestResponsesToolIdentityAcrossEvents(t *testing.T) {
	ids := responseIDs{}
	var itemID, callID string
	for _, typ := range []string{"response.output_item.added", "response.output_item.done", "response.completed"} {
		item := map[string]any{"type": "function_call", "name": "read_file", "arguments": "{}"}
		event := map[string]any{"type": typ, "output_index": 0, "item": item}
		if typ == "response.completed" {
			event = map[string]any{"type": typ, "response": map[string]any{"output": []any{item}}}
		}
		if err := ids.normalize(event); err != nil {
			t.Fatal(err)
		}
		if itemID == "" {
			itemID, callID = stringValue(item["id"]), stringValue(item["call_id"])
		}
		if item["id"] != itemID || item["call_id"] != callID {
			t.Fatalf("identity changed: %#v", item)
		}
		// The existing field patcher must not replace the assigned identities.
		encoded, err := encodeRequestObject(event)
		if err != nil {
			t.Fatal(err)
		}
		patched := patch.PatchSSEDataLineWithSequence("data: "+string(encoded), patch.Options{}, 0)
		if !strings.Contains(patched, itemID) || !strings.Contains(patched, callID) {
			t.Fatal(patched)
		}
	}
	delta := map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "{}"}
	if err := ids.normalize(delta); err != nil {
		t.Fatal(err)
	}
	if delta["item_id"] != itemID {
		t.Fatal("arguments lost their target")
	}
	conflict := map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "different"}}
	if err := ids.normalize(conflict); err == nil {
		t.Fatal("conflicting ID accepted")
	}
	missingIndex := map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call"}}
	if err := ids.normalize(missingIndex); err == nil {
		t.Fatal("ambiguous index accepted")
	}
}

func TestResponsesDuplicateItemIDRemapped(t *testing.T) {
	ids := responseIDs{}
	first := map[string]any{"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "reasoning", "id": "rs_shared", "status": "completed"}}
	if err := ids.normalize(first); err != nil {
		t.Fatal(err)
	}
	second := map[string]any{"type": "response.output_item.added", "output_index": 1,
		"item": map[string]any{"type": "reasoning", "id": "rs_shared", "status": "in_progress"}}
	if err := ids.normalize(second); err != nil {
		t.Fatalf("duplicate id rejected instead of remapped: %v", err)
	}
	remapped := stringValue(second["item"].(map[string]any)["id"])
	if remapped == "" || remapped == "rs_shared" {
		t.Fatalf("id not remapped: %#v", second)
	}
	if !strings.HasPrefix(remapped, "rs_") {
		t.Fatalf("type prefix lost: %s", remapped)
	}
	// Later events for the second slot must keep the remapped id.
	delta := map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 1, "item_id": "rs_shared", "delta": "x"}
	if err := ids.normalize(delta); err != nil {
		t.Fatal(err)
	}
	if delta["item_id"] != remapped {
		t.Fatalf("delta item_id=%v want %s", delta["item_id"], remapped)
	}
	// A completed snapshot carrying both items must stay consistent.
	completed := map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{
		map[string]any{"type": "reasoning", "id": "rs_shared"},
		map[string]any{"type": "reasoning", "id": "rs_shared"},
	}}}
	if err := ids.normalize(completed); err != nil {
		t.Fatal(err)
	}
	output := anySlice(completed["response"].(map[string]any)["output"])
	if stringValue(output[0].(map[string]any)["id"]) != "rs_shared" || stringValue(output[1].(map[string]any)["id"]) != remapped {
		t.Fatalf("completed output ids inconsistent: %#v", output)
	}
	if ids.remapped == 0 {
		t.Fatal("remap not counted")
	}
}

func TestMessagesMissingToolIDsBeforeValidation(t *testing.T) {
	first := map[string]any{"type": "tool_use", "name": "read_file", "input": map[string]any{}}
	second := map[string]any{"type": "tool_use", "name": "read_file", "input": map[string]any{}}
	root := map[string]any{"content": []any{first, second}}
	normalizeMessagesToolIDs(root)
	id := first["id"]
	if id == "" || id == second["id"] {
		t.Fatal("missing or repeated ID")
	}
	normalizeMessagesToolIDs(root)
	if first["id"] != id {
		t.Fatal("normalization changed ID")
	}
	if err := validateMessagesContentBlock(first); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "name": "read_file"}}
	normalizeMessagesStreamRequiredFields(start)
	if err := validateMessagesContentBlock(start["content_block"].(map[string]any)); err != nil {
		t.Fatal(err)
	}
}

func TestChatJSONFallbackAssignsParallelToolIndexes(t *testing.T) {
	root, err := decodeJSONMap([]byte(`{"id":"chat_1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"read_file","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := writeChatSSEFallback(w, root); err != nil {
		t.Fatal(err)
	}
	var count int
	err = scanSSEPayloads(strings.NewReader(w.Body.String()), func(_ []string, payload []byte) error {
		if string(payload) == "[DONE]" {
			return nil
		}
		chunk, err := decodeJSONMap(payload)
		if err != nil {
			return err
		}
		choice := anySlice(chunk["choices"])[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		for index, raw := range anySlice(delta["tool_calls"]) {
			call := raw.(map[string]any)
			if _, present, valid := optionalCanonicalToken(call, "index"); !present || !valid || numberInt(call["index"]) != index {
				t.Fatal("incorrect tool index")
			}
			count++
		}
		return validateNativeChatChunk(chunk)
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("received %d calls", count)
	}
}

func TestChatJSONFallbackEmitsReasoningBeforeContent(t *testing.T) {
	root, err := decodeJSONMap([]byte(`{"id":"chat_1","object":"chat.completion","created":1,"model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"没有。","reasoning_content":"checking git. previous task (x) is complete. The user says \"continue. If all tasks are complete, reply only: 任务已全部完成\"."},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	if err := writeChatSSEFallback(w, root); err != nil {
		t.Fatal(err)
	}
	var reasoning, text []string
	sawText := false
	err = scanSSEPayloads(strings.NewReader(w.Body.String()), func(_ []string, payload []byte) error {
		if string(payload) == "[DONE]" {
			return nil
		}
		chunk, err := decodeJSONMap(payload)
		if err != nil {
			return err
		}
		if err := validateNativeChatChunk(chunk); err != nil {
			return err
		}
		choice := anySlice(chunk["choices"])[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if thought := firstString(delta, "reasoning_content", "reasoning"); thought != "" {
			if sawText {
				return fmt.Errorf("reasoning after content")
			}
			reasoning = append(reasoning, thought)
		}
		if piece := chatMessageText(delta["content"]); piece != "" {
			sawText = true
			text = append(text, piece)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(reasoning, "") != "checking git." {
		t.Fatalf("reasoning=%q body=%s", reasoning, w.Body.String())
	}
	if strings.Join(text, "") != "没有。" {
		t.Fatalf("text=%q", text)
	}
	if strings.Contains(w.Body.String(), "任务已全部完成") {
		t.Fatalf("protocol-meta leaked: %s", w.Body.String())
	}
}
