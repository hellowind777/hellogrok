package proxy

import (
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
