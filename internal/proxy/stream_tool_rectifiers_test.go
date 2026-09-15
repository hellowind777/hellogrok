package proxy

import (
	"testing"
)

func messagesFrame(typ string, index int, extra map[string]any) map[string]any {
	frame := map[string]any{"type": typ, "index": index}
	for k, v := range extra {
		frame[k] = v
	}
	return frame
}

func TestMessagesToolRectifierResolvesDroppedNameAtStop(t *testing.T) {
	rectifier := newMessagesToolRectifier(shapeTieTools())
	frames := rectifier.ingest(messagesFrame("content_block_start", 0, map[string]any{
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "", "input": map[string]any{}},
	}))
	if len(frames) != 0 {
		t.Fatalf("start frame not held: %s", mustJSON(frames))
	}
	frames = rectifier.ingest(messagesFrame("content_block_delta", 0, map[string]any{
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `command":"pwd","description":"list"}`},
	}))
	if len(frames) != 0 {
		t.Fatalf("delta frame not held: %s", mustJSON(frames))
	}
	frames = rectifier.ingest(messagesFrame("content_block_stop", 0, nil))
	if len(frames) != 3 {
		t.Fatalf("expected start+delta+stop, got %d: %s", len(frames), mustJSON(frames))
	}
	for _, frame := range frames {
		if err := validateNativeSSEFrame(wireMessages, frame); err != nil {
			t.Fatalf("emitted frame invalid: %v frame=%s", err, mustJSON(frame))
		}
	}
	start, _ := frames[0]["content_block"].(map[string]any)
	if stringValue(start["name"]) != "run_terminal_command" {
		t.Fatalf("name not resolved: %#v", start)
	}
	delta, _ := frames[1]["delta"].(map[string]any)
	if !jsonObjectComplete(stringValue(delta["partial_json"])) {
		t.Fatalf("arguments not repaired: %#v", delta)
	}
	if stringValue(frames[2]["type"]) != "content_block_stop" {
		t.Fatalf("stop frame missing: %s", mustJSON(frames))
	}
}

func TestMessagesToolRectifierFlushesHeldBlocksAtMessageStop(t *testing.T) {
	rectifier := newMessagesToolRectifier(shapeTieTools())
	rectifier.ingest(messagesFrame("content_block_start", 0, map[string]any{
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read_file", "input": map[string]any{}},
	}))
	rectifier.ingest(messagesFrame("content_block_delta", 0, map[string]any{
		"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"target_file":"a.py"}`},
	}))
	frames := rectifier.ingest(map[string]any{"type": "message_stop"})
	if len(frames) != 3 {
		t.Fatalf("held block not flushed before stop: %d %s", len(frames), mustJSON(frames))
	}
	if stringValue(frames[2]["type"]) != "message_stop" {
		t.Fatalf("message_stop not last: %s", mustJSON(frames))
	}
}

func TestMessagesToolRectifierPassesThroughTextBlocks(t *testing.T) {
	rectifier := newMessagesToolRectifier(shapeTieTools())
	frames := rectifier.ingest(messagesFrame("content_block_start", 1, map[string]any{
		"content_block": map[string]any{"type": "text", "text": ""},
	}))
	if len(frames) != 1 {
		t.Fatalf("text block was held: %s", mustJSON(frames))
	}
}

func responsesFrame(typ string, index int, extra map[string]any) map[string]any {
	frame := map[string]any{"type": typ, "output_index": index}
	for k, v := range extra {
		frame[k] = v
	}
	return frame
}

func TestResponsesToolRectifierResolvesDroppedNameAtDone(t *testing.T) {
	rectifier := newResponsesToolRectifier(shapeTieTools())
	frames := rectifier.ingest(responsesFrame("response.output_item.added", 0, map[string]any{
		"item": map[string]any{"type": "function_call", "id": "call_1", "name": "", "arguments": ""},
	}))
	if len(frames) != 0 {
		t.Fatalf("added frame not held: %s", mustJSON(frames))
	}
	frames = rectifier.ingest(responsesFrame("response.function_call_arguments.delta", 0, map[string]any{
		"item_id": "call_1", "delta": `command":"pwd","description":"list"}`,
	}))
	if len(frames) != 0 {
		t.Fatalf("arguments delta not held: %s", mustJSON(frames))
	}
	frames = rectifier.ingest(responsesFrame("response.output_item.done", 0, map[string]any{
		"item": map[string]any{"type": "function_call", "id": "call_1", "name": "", "arguments": ""},
	}))
	if len(frames) != 3 {
		t.Fatalf("expected added+delta+done, got %d: %s", len(frames), mustJSON(frames))
	}
	addedItem, _ := frames[0]["item"].(map[string]any)
	if stringValue(addedItem["name"]) != "run_terminal_command" {
		t.Fatalf("added name not resolved: %#v", addedItem)
	}
	if !jsonObjectComplete(stringValue(frames[1]["delta"])) {
		t.Fatalf("arguments delta not repaired: %#v", frames[1])
	}
	doneItem, _ := frames[2]["item"].(map[string]any)
	if stringValue(doneItem["name"]) != "run_terminal_command" {
		t.Fatalf("done item name not resolved: %#v", doneItem)
	}
}

func TestResponsesToolRectifierPrefersCompleteDoneArguments(t *testing.T) {
	rectifier := newResponsesToolRectifier(shapeTieTools())
	rectifier.ingest(responsesFrame("response.output_item.added", 0, map[string]any{
		"item": map[string]any{"type": "function_call", "id": "call_1", "name": "run_terminal_command", "arguments": ""},
	}))
	rectifier.ingest(responsesFrame("response.function_call_arguments.delta", 0, map[string]any{
		"item_id": "call_1", "delta": `{"command":"git sta`,
	}))
	frames := rectifier.ingest(responsesFrame("response.output_item.done", 0, map[string]any{
		"item": map[string]any{"type": "function_call", "id": "call_1", "name": "run_terminal_command", "arguments": `{"command":"git status"}`},
	}))
	var delta string
	for _, frame := range frames {
		if stringValue(frame["type"]) == "response.function_call_arguments.delta" {
			delta = stringValue(frame["delta"])
		}
	}
	if !jsonObjectComplete(delta) {
		t.Fatalf("truncated stream args won over complete done args: %q frames=%s", delta, mustJSON(frames))
	}
	obj := parseToolArguments(delta)
	if stringArg(obj, "command") != "git status" {
		t.Fatalf("command lost: %#v", obj)
	}
}

func TestResponsesToolRectifierFlushesHeldItemsAtTerminal(t *testing.T) {
	rectifier := newResponsesToolRectifier(shapeTieTools())
	rectifier.ingest(responsesFrame("response.output_item.added", 0, map[string]any{
		"item": map[string]any{"type": "function_call", "id": "call_1", "name": "read_file", "arguments": ""},
	}))
	frames := rectifier.ingest(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{}}})
	if len(frames) != 2 {
		t.Fatalf("held item not flushed before terminal: %d %s", len(frames), mustJSON(frames))
	}
	if stringValue(frames[1]["type"]) != "response.completed" {
		t.Fatalf("terminal not last: %s", mustJSON(frames))
	}
}

func TestStreamToolRectifiersIgnoreForeignFrames(t *testing.T) {
	messages := newMessagesToolRectifier(shapeTieTools())
	if frames := messages.ingest(map[string]any{"type": "message_delta"}); len(frames) != 1 {
		t.Fatalf("messages foreign frame altered: %s", mustJSON(frames))
	}
	responses := newResponsesToolRectifier(shapeTieTools())
	if frames := responses.ingest(map[string]any{"type": "response.output_text.delta"}); len(frames) != 1 {
		t.Fatalf("responses foreign frame altered: %s", mustJSON(frames))
	}
	if frames := responses.ingest(nil); len(frames) != 1 {
		t.Fatalf("nil root not passed through: %d", len(frames))
	}
}
