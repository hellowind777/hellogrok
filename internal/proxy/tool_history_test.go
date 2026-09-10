package proxy

import "testing"

func TestChatCallIDsAcrossDeltasAndResponses(t *testing.T) {
	ids := chatCallIDs{}
	chunk := func(id string) (map[string]any, map[string]any) {
		call := map[string]any{"index": 0, "id": id, "function": map[string]any{"arguments": "{}"}}
		return map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}}}, call
	}
	first, call := chunk("")
	if err := ids.normalize(first, true); err != nil {
		t.Fatal(err)
	}
	id := stringValue(call["id"])
	if id == "" {
		t.Fatal("missing generated ID")
	}
	late, lateCall := chunk("provider_late")
	if err := ids.normalize(late, true); err != nil {
		t.Fatal(err)
	}
	if lateCall["id"] != id {
		t.Fatal("ID changed after delivery")
	}
	next, nextCall := chunk("")
	if err := (chatCallIDs{}).normalize(next, true); err != nil {
		t.Fatal(err)
	}
	if nextCall["id"] == id {
		t.Fatal("ID reused across responses")
	}
}

func TestChatHistoryRejectsAmbiguousRepair(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{}, map[string]any{}}},
		map[string]any{"role": "tool", "tool_call_id": "one"},
		map[string]any{"role": "tool", "tool_call_id": "two"},
	}
	if err := normalizeChatToolHistory(messages); err == nil {
		t.Fatal("ambiguous history accepted")
	}
}

func TestNormalizeChatToolHistoryAddsStableIDs(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "inspect"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "arguments": "{}"}},
			map[string]any{"type": "function", "id": "provider_call", "function": map[string]any{"name": "list_dir", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1_0", "content": "ok"},
		map[string]any{"role": "tool", "tool_call_id": "provider_call", "content": "ok"},
	}
	if err := normalizeChatToolHistory(messages); err != nil {
		t.Fatal(err)
	}
	calls := anySlice(messages[1].(map[string]any)["tool_calls"])
	if got := stringValue(calls[0].(map[string]any)["id"]); got != "call_1_0" {
		t.Fatalf("generated id = %q", got)
	}
	if got := stringValue(calls[1].(map[string]any)["id"]); got != "provider_call" {
		t.Fatalf("provider id changed to %q", got)
	}
	if err := validateChatToolHistory(messages); err != nil {
		t.Fatalf("normalized history rejected: %v", err)
	}
}

func TestNormalizeChatToolHistoryDoesNotReplaceExistingIDs(t *testing.T) {
	messages := []any{map[string]any{"role": "assistant", "tool_calls": []any{
		map[string]any{"id": "call_1", "type": "function"},
	}}}
	if err := normalizeChatToolHistory(messages); err != nil {
		t.Fatal(err)
	}
	if got := stringValue(anySlice(messages[0].(map[string]any)["tool_calls"])[0].(map[string]any)["id"]); got != "call_1" {
		t.Fatalf("existing id changed to %q", got)
	}
}
